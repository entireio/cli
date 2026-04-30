package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"golang.org/x/sync/errgroup"
)

var (
	lookupCurrentToken = auth.LookupCurrentToken
	nowUTC             = func() time.Time { return time.Now().UTC() }
)

func runLocal(ctx context.Context, opts Options) (*Dispatch, error) {
	now := nowUTC()
	sinceInput := strings.TrimSpace(opts.Since)
	if sinceInput == "" {
		sinceInput = "7d"
	}
	since, err := ParseSinceAtNow(sinceInput, now)
	if err != nil {
		return nil, err
	}
	until, err := ParseUntilAtNow(opts.Until, now)
	if err != nil {
		return nil, err
	}
	normalizedSince, normalizedUntil := NormalizeWindow(since, until)
	if !normalizedSince.Before(normalizedUntil) {
		return nil, errors.New("--since must be before --until")
	}

	repoRoots, err := resolveRepoRoots(ctx, opts.RepoPaths)
	if err != nil {
		return nil, err
	}

	allCandidates := make([]candidate, 0)
	var candidatesMu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	for _, repoRoot := range repoRoots {
		group.Go(func() error {
			candidates, err := enumerateRepoCandidates(groupCtx, repoRoot, opts, normalizedSince, normalizedUntil)
			if err != nil {
				return err
			}
			candidatesMu.Lock()
			allCandidates = append(allCandidates, candidates...)
			candidatesMu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("enumerate repo candidates: %w", err)
	}

	fallback := applyFallbackChain(allCandidates)
	dispatch := &Dispatch{
		CoveredRepos: coveredRepos(allCandidates),
		Repos:        groupBulletsByRepo(fallback.Used),
		Window: Window{
			NormalizedSince:   normalizedSince,
			NormalizedUntil:   normalizedUntil,
			FirstCheckpointAt: firstAt(fallback.Used),
			LastCheckpointAt:  lastAt(fallback.Used),
		},
	}

	text, err := generateLocalDispatch(ctx, dispatch, opts.Voice)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, errDispatchMissingMarkdown
	}
	dispatch.GeneratedText = text

	return dispatch, nil
}

func NormalizeWindow(since, until time.Time) (time.Time, time.Time) {
	floored := since.Truncate(time.Minute)
	ceiled := until.Truncate(time.Minute)
	if !until.Equal(ceiled) {
		ceiled = ceiled.Add(time.Minute)
	}
	return floored, ceiled
}

func resolveRepoRoots(ctx context.Context, repoPaths []string) ([]string, error) {
	if len(repoPaths) == 0 {
		repoRoot, err := paths.WorktreeRoot(ctx)
		if err != nil {
			return nil, fmt.Errorf("not in a git repository: %w", err)
		}
		return []string{repoRoot}, nil
	}

	roots := make([]string, 0, len(repoPaths))
	for _, repoPath := range repoPaths {
		cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--show-toplevel")
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("resolve repo root for %q: %w", repoPath, err)
		}
		roots = append(roots, strings.TrimSpace(string(output)))
	}
	return roots, nil
}

func enumerateRepoCandidates(ctx context.Context, repoRoot string, opts Options, since, until time.Time) ([]candidate, error) {
	repo, err := git.PlainOpenWithOptions(repoRoot, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, fmt.Errorf("open repository %s: %w", repoRoot, err)
	}

	repoFullName, err := resolveRepoFullName(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("resolve repo name for %s: %w", repoRoot, err)
	}

	branches := opts.Branches
	switch {
	case opts.AllBranches:
		branches, err = localBranchNames(repo)
		if err != nil {
			return nil, err
		}
	case len(branches) == 0:
		currentBranch, err := currentBranchName(repo)
		if err != nil {
			return nil, err
		}
		branches = []string{currentBranch}
	}
	branchSet := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		branchSet[branch] = struct{}{}
	}
	reachableCheckpointIDs := map[string]struct{}{}
	if opts.ImplicitCurrentBranch && !opts.AllBranches {
		reachableCheckpointIDs, err = reachableCheckpointIDsInRange(ctx, repoRoot, branchLocalRevRange(ctx, repoRoot), since)
		if err != nil {
			return nil, err
		}
	}
	commitSubjectsByCheckpoint, err := loadCommitSubjectsByCheckpoint(ctx, repoRoot, since)
	if err != nil {
		return nil, err
	}

	store := checkpoint.NewGitStore(repo)
	infos, err := store.ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("list committed checkpoints: %w", err)
	}

	candidates := make([]candidate, 0, len(infos))
	for _, info := range infos {
		if info.CreatedAt.Before(since) || !info.CreatedAt.Before(until) {
			continue
		}

		summary, err := store.ReadCommitted(ctx, info.CheckpointID)
		if err != nil {
			logging.Warn(ctx, "failed to read committed checkpoint for dispatch", "checkpoint_id", info.CheckpointID.String(), "error", err)
			continue
		}
		if summary == nil {
			continue
		}
		if _, onSelectedBranch := branchSet[summary.Branch]; !onSelectedBranch {
			if !opts.ImplicitCurrentBranch {
				continue
			}
			if _, reachable := reachableCheckpointIDs[info.CheckpointID.String()]; !reachable {
				continue
			}
		}

		localSummary := ""
		if len(summary.Sessions) > 0 {
			latestIndex := len(summary.Sessions) - 1
			if metadata, err := store.ReadSessionMetadata(ctx, info.CheckpointID, latestIndex); err == nil && metadata != nil && metadata.Summary != nil {
				localSummary = strings.TrimSpace(metadata.Summary.Outcome)
				if localSummary == "" {
					localSummary = strings.TrimSpace(metadata.Summary.Intent)
				}
			}
		}

		commitSubject := commitSubjectsByCheckpoint[info.CheckpointID.String()]
		candidates = append(candidates, candidate{
			CheckpointID:      info.CheckpointID.String(),
			RepoFullName:      repoFullName,
			Branch:            summary.Branch,
			CreatedAt:         info.CreatedAt,
			CommitSubject:     commitSubject,
			LocalSummaryTitle: localSummary,
		})
	}

	return candidates, nil
}

func reachableCheckpointIDsInRange(ctx context.Context, repoRoot, revRange string, since time.Time) (map[string]struct{}, error) {
	cmd := exec.CommandContext(
		ctx,
		"git",
		"-C",
		repoRoot,
		"log",
		revRange,
		"--since="+since.UTC().Format(time.RFC3339),
		"--grep",
		"Entire-Checkpoint:",
		"--format=%B%x00",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list HEAD checkpoint trailers: %w", err)
	}

	reachable := make(map[string]struct{})
	for _, message := range strings.Split(string(output), "\x00") {
		for _, checkpointID := range trailers.ParseAllCheckpoints(message) {
			reachable[checkpointID.String()] = struct{}{}
		}
	}
	return reachable, nil
}

// branchLocalRevRange returns a git log rev range that limits commits to
// those unique to the current branch — reachable from HEAD but not from the
// repository's default branch. Falls back to "HEAD" when no default branch
// can be resolved (e.g. a fresh repo with no main/master ref).
func branchLocalRevRange(ctx context.Context, repoRoot string) string {
	base := defaultBranchRef(ctx, repoRoot)
	if base == "" {
		return "HEAD"
	}
	return base + "..HEAD"
}

// defaultBranchRef resolves the repository's default branch, preferring
// origin/HEAD and falling back to the conventional main/master names. A
// candidate is only accepted if it is an ancestor of HEAD — otherwise a
// repo with an unconventional default (e.g. develop, trunk) would silently
// exclude the wrong history. Returns an empty string if nothing matches so
// callers can skip the exclusion.
func defaultBranchRef(ctx context.Context, repoRoot string) string {
	if out, ok := runGitOutput(ctx, repoRoot, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); ok {
		ref := strings.TrimSpace(out)
		if strings.HasPrefix(ref, "refs/remotes/") {
			candidate := strings.TrimPrefix(ref, "refs/remotes/")
			if isAncestorOfHEAD(ctx, repoRoot, candidate) {
				return candidate
			}
		}
	}
	for _, candidate := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, ok := runGitOutput(ctx, repoRoot, "rev-parse", "--verify", "--quiet", candidate); !ok {
			continue
		}
		if isAncestorOfHEAD(ctx, repoRoot, candidate) {
			return candidate
		}
	}
	return ""
}

func isAncestorOfHEAD(ctx context.Context, repoRoot, ref string) bool {
	_, ok := runGitOutput(ctx, repoRoot, "merge-base", "--is-ancestor", ref, "HEAD")
	return ok
}

func runGitOutput(ctx context.Context, repoRoot string, args ...string) (string, bool) {
	fullArgs := append([]string{"-C", repoRoot}, args...)
	out, err := exec.CommandContext(ctx, "git", fullArgs...).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func resolveRepoFullName(ctx context.Context, repo *git.Repository) (string, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		logging.Warn(ctx, "dispatch repo resolution failed", "step", "origin_remote", "error", err)
		return "", fmt.Errorf("dispatch currently supports GitHub repositories with an origin remote: %w", err)
	}
	if len(remote.Config().URLs) == 0 {
		return "", errors.New("dispatch currently supports GitHub repositories with an origin remote URL")
	}

	owner, repoName, err := search.ParseGitHubRemote(remote.Config().URLs[0])
	if err != nil {
		logging.Warn(ctx, "dispatch repo resolution failed", "step", "parse_origin_remote", "error", err)
		return "", fmt.Errorf("dispatch currently supports GitHub origin remotes only: %w", err)
	}
	return owner + "/" + repoName, nil
}

func currentBranchName(repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("get HEAD: %w", err)
	}
	if !head.Name().IsBranch() {
		return "", errors.New("not on a branch (detached HEAD)")
	}
	return head.Name().Short(), nil
}

// localBranchNames returns the short names of the user's local branches,
// omitting the reserved "entire/" namespace used for internal refs.
func localBranchNames(repo *git.Repository) ([]string, error) {
	iter, err := repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("list local branches: %w", err)
	}
	defer iter.Close()

	var names []string
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if strings.HasPrefix(name, checkpoint.ShadowBranchPrefix) {
			return nil
		}
		names = append(names, name)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("iterate local branches: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

func loadCommitSubjectsByCheckpoint(ctx context.Context, repoRoot string, since time.Time) (map[string]string, error) {
	cmd := exec.CommandContext(
		ctx,
		"git",
		"-C",
		repoRoot,
		"log",
		"--all",
		"--since="+since.UTC().Format(time.RFC3339),
		"--grep",
		"Entire-Checkpoint:",
		"--format=%s%x00%B%x00%x00",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log --grep: %w", err)
	}

	subjects := make(map[string]string)
	for _, record := range strings.Split(string(output), "\x00\x00") {
		record = strings.TrimSuffix(record, "\x00")
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		subject := strings.TrimSpace(parts[0])
		for _, checkpointID := range trailers.ParseAllCheckpoints(parts[1]) {
			id := checkpointID.String()
			if _, ok := subjects[id]; ok {
				continue
			}
			subjects[id] = subject
		}
	}
	return subjects, nil
}

func coveredRepos(candidates []candidate) []string {
	if len(candidates) == 0 {
		return nil
	}

	repoSet := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.RepoFullName == "" {
			continue
		}
		repoSet[candidate.RepoFullName] = struct{}{}
	}

	repos := make([]string, 0, len(repoSet))
	for repoFullName := range repoSet {
		repos = append(repos, repoFullName)
	}
	sort.Strings(repos)
	return repos
}

func groupBulletsByRepo(used []repoBullet) []RepoGroup {
	repoMap := make(map[string]map[string][]Bullet)
	for _, item := range used {
		if _, ok := repoMap[item.RepoFullName]; !ok {
			repoMap[item.RepoFullName] = make(map[string][]Bullet)
		}
		label := "Updates"
		if len(item.Bullet.Labels) > 0 && strings.TrimSpace(item.Bullet.Labels[0]) != "" {
			label = item.Bullet.Labels[0]
		}
		repoMap[item.RepoFullName][label] = append(repoMap[item.RepoFullName][label], item.Bullet)
	}

	repoNames := make([]string, 0, len(repoMap))
	for repoName := range repoMap {
		repoNames = append(repoNames, repoName)
	}
	sort.Strings(repoNames)

	out := make([]RepoGroup, 0, len(repoNames))
	for _, repoName := range repoNames {
		sectionMap := repoMap[repoName]
		labels := make([]string, 0, len(sectionMap))
		for label := range sectionMap {
			labels = append(labels, label)
		}
		sort.Strings(labels)

		sections := make([]Section, 0, len(labels))
		for _, label := range labels {
			sections = append(sections, Section{
				Label:   label,
				Bullets: sectionMap[label],
			})
		}
		out = append(out, RepoGroup{
			FullName: repoName,
			URL:      githubRepoURL(repoName),
			Sections: sections,
		})
	}

	return out
}

func firstAt(used []repoBullet) time.Time {
	var first time.Time
	for _, item := range used {
		if item.Bullet.CreatedAt.IsZero() {
			continue
		}
		if first.IsZero() || item.Bullet.CreatedAt.Before(first) {
			first = item.Bullet.CreatedAt
		}
	}
	return first
}

func lastAt(used []repoBullet) time.Time {
	var last time.Time
	for _, item := range used {
		if item.Bullet.CreatedAt.After(last) {
			last = item.Bullet.CreatedAt
		}
	}
	return last
}
