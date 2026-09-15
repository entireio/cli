package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"golang.org/x/sync/errgroup"
)

var (
	// lookupResourceToken returns a bearer for the given data-API base URL.
	// Production wiring goes through auth.ResolveDataAPIToken so the dispatch
	// host's /.well-known/entire-api.json picks the matching login context
	// (a host that doesn't advertise discovery is a surfaced error). Tests
	// swap to a fixed-token closure.
	lookupResourceToken = auth.ResolveDataAPIToken

	nowUTC = func() time.Time { return time.Now().UTC() }
)

type localPreflight struct {
	normalizedSince time.Time
	normalizedUntil time.Time
	repoRoots       []string
	sinceInput      string
	untilInput      string
	repoPathsInput  []string
}

func (p *localPreflight) matches(opts Options) bool {
	return p != nil &&
		p.sinceInput == opts.Since &&
		p.untilInput == opts.Until &&
		slices.Equal(p.repoPathsInput, opts.RepoPaths)
}

// PrepareLocal validates and resolves the inputs needed before local dispatch
// generation can begin. The returned options can be passed to Run without
// repeating time-window parsing or repository-root discovery.
func PrepareLocal(ctx context.Context, opts Options) (Options, error) {
	if opts.Mode != ModeLocal {
		return Options{}, errors.New("local dispatch preflight requires local mode")
	}

	now := nowUTC()
	sinceInput := strings.TrimSpace(opts.Since)
	if sinceInput == "" {
		sinceInput = "7d"
	}
	since, err := ParseSinceAtNow(sinceInput, now)
	if err != nil {
		return Options{}, err
	}
	until, err := ParseUntilAtNow(opts.Until, now)
	if err != nil {
		return Options{}, err
	}
	normalizedSince, normalizedUntil := NormalizeWindow(since, until)
	if !normalizedSince.Before(normalizedUntil) {
		return Options{}, errors.New("--since must be before --until")
	}

	repoRoots, err := resolveRepoRoots(ctx, opts.RepoPaths)
	if err != nil {
		return Options{}, err
	}

	opts.localPreflight = &localPreflight{
		normalizedSince: normalizedSince,
		normalizedUntil: normalizedUntil,
		repoRoots:       repoRoots,
		sinceInput:      opts.Since,
		untilInput:      opts.Until,
		repoPathsInput:  slices.Clone(opts.RepoPaths),
	}
	return opts, nil
}

func runLocal(ctx context.Context, opts Options) (*Dispatch, error) {
	if !opts.localPreflight.matches(opts) {
		prepared, err := PrepareLocal(ctx, opts)
		if err != nil {
			return nil, err
		}
		opts = prepared
	}
	preflight := opts.localPreflight

	allCandidates := make([]candidate, 0)
	var candidatesMu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	for _, repoRoot := range preflight.repoRoots {
		group.Go(func() error {
			candidates, err := enumerateRepoCandidates(groupCtx, repoRoot, opts, preflight.normalizedSince, preflight.normalizedUntil)
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
			NormalizedSince:   preflight.normalizedSince,
			NormalizedUntil:   preflight.normalizedUntil,
			FirstCheckpointAt: firstAt(fallback.Used),
			LastCheckpointAt:  lastAt(fallback.Used),
		},
	}

	text, err := generateLocalDispatch(ctx, dispatch, opts.Voice, opts.TextGenerator, opts.Model)
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
	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open repository %s: %w", repoRoot, err)
	}
	defer repo.Close()

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
	reachableCheckpointIDs := map[string]time.Time{}
	if opts.ImplicitCurrentBranch && !opts.AllBranches {
		currentBranch := ""
		if len(branches) > 0 {
			currentBranch = branches[0]
		}
		reachableCheckpointIDs, err = reachableCheckpointIDsInRange(ctx, repoRoot, branchLocalRevRange(ctx, repoRoot, currentBranch), since, until)
		if err != nil {
			return nil, err
		}
	}
	commitSubjectsByCheckpoint, err := loadCommitSubjectsByCheckpoint(ctx, repoRoot, since)
	if err != nil {
		return nil, err
	}

	// repoRoot may be a different repo (--repo/RepoPaths) or the cwd may not be
	// a repo at all, so scope checkpoint store construction to this repo.
	repoCtx := settings.WithWorktreeRoot(ctx, repoRoot)
	stores, err := checkpoint.Open(repoCtx, repo, checkpoint.OpenOptions{ReadRemotes: strategy.CheckpointReadRemotes(repoCtx)})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	store := stores.Persistent
	infos, err := store.List(repoCtx)
	if err != nil {
		return nil, fmt.Errorf("list committed checkpoints: %w", err)
	}

	candidates := make([]candidate, 0, len(infos))
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info.CreatedAt.Before(since) || !info.CreatedAt.Before(until) {
			continue
		}

		summary, err := store.Read(repoCtx, info.CheckpointID)
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

		commitSubject := commitSubjectsByCheckpoint[info.CheckpointID.String()]
		candidates = append(candidates, candidate{
			CheckpointID:      info.CheckpointID.String(),
			RepoFullName:      repoFullName,
			Branch:            summary.Branch,
			CreatedAt:         info.CreatedAt,
			CommitSubject:     commitSubject,
			LocalSummaryTitle: readLocalSummaryTitle(repoCtx, store, info.CheckpointID, summary),
		})
		seen[info.CheckpointID.String()] = struct{}{}
	}

	// Second pass: checkpoints referenced by branch commit trailers in the
	// window that store.List did not surface. store.List only enumerates
	// checkpoints present in the local checkout, but on a checkout that has not
	// fetched recent checkpoints (the common case — checkpoints are pushed to
	// the remote from other worktrees) the recent work is missing locally even
	// though it is reachable from HEAD. The commit subject is always available
	// from git log, so we can summarize that work from the trailer + subject
	// without a (slow) per-checkpoint network fetch; when the checkpoint does
	// happen to be local we still prefer its richer session summary. Windowed
	// by commit ("landed on branch") time, since a CheckpointSummary carries no
	// CreatedAt of its own.
	for idStr, commitTime := range reachableCheckpointIDs {
		if _, ok := seen[idStr]; ok {
			continue
		}
		if commitTime.Before(since) || !commitTime.Before(until) {
			continue
		}
		commitSubject := commitSubjectsByCheckpoint[idStr]

		// Opportunistic local read for a richer title/branch — no fetcher is
		// wired, so this never touches the network; a checkpoint absent locally
		// resolves to nil and we fall back to the commit subject.
		branch := ""
		localSummary := ""
		if cid, cidErr := checkpointid.NewCheckpointID(idStr); cidErr == nil {
			if summary, readErr := store.Read(repoCtx, cid); readErr == nil && summary != nil {
				branch = summary.Branch
				localSummary = readLocalSummaryTitle(repoCtx, store, cid, summary)
			}
		}

		if strings.TrimSpace(localSummary) == "" && strings.TrimSpace(commitSubject) == "" {
			continue
		}
		candidates = append(candidates, candidate{
			CheckpointID:      idStr,
			RepoFullName:      repoFullName,
			Branch:            branch,
			CreatedAt:         commitTime,
			CommitSubject:     commitSubject,
			LocalSummaryTitle: localSummary,
		})
		seen[idStr] = struct{}{}
	}

	// The second pass ranges over a map (randomized iteration order), so sort
	// before returning to keep bullet order — and therefore the LLM-authored
	// summary — stable across runs. Newest first, with the checkpoint ID as a
	// deterministic tiebreak for equal timestamps.
	sortCandidatesByRecency(candidates)
	return candidates, nil
}

// sortCandidatesByRecency orders candidates most-recent-first by CreatedAt,
// breaking ties by checkpoint ID so the order is fully deterministic.
func sortCandidatesByRecency(candidates []candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
		}
		return candidates[i].CheckpointID < candidates[j].CheckpointID
	})
}

// readLocalSummaryTitle returns the latest session's outcome (falling back to
// its intent) for use as a bullet title, or "" when no session summary is
// available.
func readLocalSummaryTitle(ctx context.Context, store checkpoint.PersistentStore, cid checkpointid.CheckpointID, summary *checkpoint.CheckpointSummary) string {
	if summary == nil || len(summary.Sessions) == 0 {
		return ""
	}
	latestIndex := len(summary.Sessions) - 1
	metadata, err := store.ReadSessionMetadata(ctx, cid, latestIndex)
	if err != nil || metadata == nil || metadata.Summary == nil {
		return ""
	}
	if outcome := strings.TrimSpace(metadata.Summary.Outcome); outcome != "" {
		return outcome
	}
	return strings.TrimSpace(metadata.Summary.Intent)
}

// reachableCheckpointIDsInRange maps each checkpoint ID referenced by a commit
// trailer in revRange, within the window [since, until), to the most recent
// referencing commit time *that falls inside the window*. The commit time is
// the "landed on this branch" timestamp, used both for membership checks and to
// window checkpoints that are fetched on demand by ID (whose CheckpointSummary
// carries no CreatedAt of its own).
//
// Commits outside the window are ignored entirely, so a checkpoint referenced
// by both an in-window commit and a later out-of-window commit still records
// its in-window time (and is therefore not dropped by the caller's window
// check). git's --since/--until only bound the scan; the explicit in-loop check
// is authoritative for the half-open [since, until) boundary.
func reachableCheckpointIDsInRange(ctx context.Context, repoRoot, revRange string, since, until time.Time) (map[string]time.Time, error) {
	cmd := exec.CommandContext(
		ctx,
		"git",
		"-C",
		repoRoot,
		"log",
		revRange,
		"--since="+since.UTC().Format(time.RFC3339),
		"--until="+until.UTC().Format(time.RFC3339),
		"--grep",
		"Entire-Checkpoint:",
		"--format=%cI%x00%B%x00%x00",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list HEAD checkpoint trailers: %w", err)
	}

	reachable := make(map[string]time.Time)
	for _, record := range strings.Split(string(output), "\x00\x00") {
		record = strings.TrimLeft(record, "\n")
		parts := strings.SplitN(record, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		commitTime, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
		if parseErr != nil {
			continue
		}
		if commitTime.Before(since) || !commitTime.Before(until) {
			continue
		}
		for _, checkpointID := range trailers.ParseAllCheckpoints(parts[1]) {
			idStr := checkpointID.String()
			if existing, ok := reachable[idStr]; !ok || commitTime.After(existing) {
				reachable[idStr] = commitTime
			}
		}
	}
	return reachable, nil
}

// branchLocalRevRange returns a git log rev range that limits commits to
// those unique to the current branch — reachable from HEAD but not from the
// repository's default branch. Falls back to "HEAD" when no default branch
// can be resolved (e.g. a fresh repo with no main/master ref).
//
// When the current branch IS the default branch there is no parent history to
// exclude: base..HEAD is empty on an up-to-date default branch, which would
// drop every checkpoint whose summary.Branch is a (now-merged) feature branch
// and leave the dispatch effectively empty. In that case we summarize
// everything reachable from HEAD in the window, matching the server-side
// dispatch. The base..HEAD exclusion only applies to feature branches.
//
// The same "nothing to exclude" reasoning holds for any branch whose base..HEAD
// range is empty — a feature-branch worktree freshly created off the up-to-date
// default branch (the standard git-worktree workflow), or a branch whose work
// has already merged into the base. There the exclusion would again drop all
// reachable in-window activity and report "no activity" despite HEAD clearly
// carrying merged work (ENT-1201). Detecting the empty range directly, rather
// than matching the default branch by name, covers both cases.
func branchLocalRevRange(ctx context.Context, repoRoot, currentBranch string) string {
	const headRev = "HEAD"
	base := defaultBranchRef(ctx, repoRoot)
	if base == "" {
		return headRev
	}
	if currentBranch != "" && strings.TrimPrefix(base, "origin/") == currentBranch {
		return headRev
	}
	if revRangeIsEmpty(ctx, repoRoot, base+".."+headRev) {
		return headRev
	}
	return base + ".." + headRev
}

// revRangeIsEmpty reports whether revRange contains no commits (i.e. HEAD has no
// commits the base lacks). A git failure is treated as non-empty so the caller
// keeps the base..HEAD exclusion — the conservative choice that never widens
// scope on error.
func revRangeIsEmpty(ctx context.Context, repoRoot, revRange string) bool {
	out, ok := runGitOutput(ctx, repoRoot, "rev-list", "--count", revRange)
	if !ok {
		return false
	}
	return strings.TrimSpace(out) == "0"
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
