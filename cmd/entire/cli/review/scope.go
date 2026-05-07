// Package review — see env.go for package-level rationale.
//
// scope.go implements scope detection for `entire review`. The scope is the
// git ref the review is bounded by: "commits unique to this branch vs
// <baseRef>". Pinning the scope at launch time prevents the divergent-default
// problem where different agents default to different comparison points (e.g.
// codex used origin/main...HEAD, claude used working-tree-only) on the same
// invocation.
package review

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// ScopeStats summarises the scope of a review for the banner.
type ScopeStats struct {
	BaseRef       string
	CurrentBranch string // empty for detached HEAD
	Commits       int    // commits unique to current branch vs BaseRef
	FilesChanged  int    // files changed across those commits
	Uncommitted   int    // uncommitted file changes in the working tree
}

// formatScopeBanner produces the line printed before agent launch:
//
//	"Reviewing feat/X vs main: 3 commits, 7 files changed, 2 uncommitted"
//
// Pluralisation: "1 commit" / "2 commits", "1 file" / "N files",
// always "uncommitted" (a count, not a noun phrase).
func formatScopeBanner(stats ScopeStats) string {
	subject := stats.CurrentBranch
	if subject == "" {
		subject = "detached HEAD"
	}

	commitWord := "commits"
	if stats.Commits == 1 {
		commitWord = "commit"
	}
	fileWord := "files"
	if stats.FilesChanged == 1 {
		fileWord = "file"
	}

	return fmt.Sprintf(
		"Reviewing %s vs %s: %d %s, %d %s changed, %d uncommitted",
		subject,
		stats.BaseRef,
		stats.Commits,
		commitWord,
		stats.FilesChanged,
		fileWord,
		stats.Uncommitted,
	)
}

// ComputeScopeStats gathers the data formatScopeBanner needs.
// Used by CU6 to build the banner before launching agents.
func ComputeScopeStats(ctx context.Context, repo *git.Repository) (ScopeStats, error) {
	baseRef, err := detectScopeBaseRef(ctx, repo)
	if err != nil {
		return ScopeStats{}, fmt.Errorf("detect scope base ref: %w", err)
	}

	repoRoot, err := repoWorktreePath(repo)
	if err != nil {
		return ScopeStats{}, fmt.Errorf("get repo root: %w", err)
	}

	currentBranch := currentBranchName(repo)

	commits, err := countCommits(ctx, repoRoot, baseRef)
	if err != nil {
		return ScopeStats{}, fmt.Errorf("count commits: %w", err)
	}

	filesChanged, err := countFilesChanged(ctx, repoRoot, baseRef)
	if err != nil {
		return ScopeStats{}, fmt.Errorf("count files changed: %w", err)
	}

	uncommitted, err := countUncommitted(ctx, repoRoot)
	if err != nil {
		return ScopeStats{}, fmt.Errorf("count uncommitted: %w", err)
	}

	return ScopeStats{
		BaseRef:       baseRef,
		CurrentBranch: currentBranch,
		Commits:       commits,
		FilesChanged:  filesChanged,
		Uncommitted:   uncommitted,
	}, nil
}

// detectScopeBaseRef finds the closest non-self ancestor branch the
// review should be scoped against. Strategy:
//
//  1. Find local + remote branches whose tips are ancestors of HEAD
//     (i.e., branches the current branch is descended from). Exclude
//     the current branch itself.
//  2. Pick the one with the most recent commit timestamp at its tip.
//     This handles stacked PRs where a feature branches off another
//     feature: prefer the immediate parent over a more distant one.
//  3. Fallback chain when no ancestor is found:
//     origin/HEAD → origin/main → origin/master → main → master
//  4. If none of those exist either, return an error.
//
// Returns the ref name (e.g., "main", "origin/main", "feat/parent")
// suitable for use in `git diff <ref>...HEAD`.
func detectScopeBaseRef(ctx context.Context, repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return fallbackScopeRef(repo)
	}
	headHash := head.Hash()

	// Determine the current branch's symbolic ref name (empty for detached HEAD).
	currentBranchShort := ""
	if head.Name().IsBranch() {
		currentBranchShort = head.Name().Short() // e.g. "feat/x"
	}

	repoRoot, rootErr := repoWorktreePath(repo)
	if rootErr != nil {
		return fallbackScopeRef(repo)
	}

	// Enumerate ancestor branches in a single git invocation. for-each-ref
	// --merged HEAD lets git use its commit-graph index to answer "is this
	// ref an ancestor of HEAD?" in O(1) per ref via packed reachability
	// rather than the previous O(branches × commits) repo.Log walks.
	type candidate struct {
		name    string
		tipUnix int64
	}
	var candidates []candidate

	out, runErr := runGit(ctx, repoRoot,
		"for-each-ref",
		"--merged", "HEAD",
		"--format=%(refname:short)%09%(committerdate:unix)",
		"refs/heads/", "refs/remotes/",
	)
	if runErr != nil {
		// git for-each-ref unavailable or repo state confused — fall back
		// to a slow walk via go-git, mirroring the prior behaviour rather
		// than failing the review launch.
		return slowDetectScopeBaseRef(ctx, repo, headHash, currentBranchShort)
	}

	for _, line := range strings.Split(out, "\n") {
		if ctx.Err() != nil {
			return "", ctx.Err() //nolint:wrapcheck // propagate context cancellation
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		// Skip the current branch itself (and its full ref form, both shapes
		// for-each-ref might emit).
		if name == currentBranchShort {
			continue
		}
		// Skip refs that resolve to the same commit as HEAD — same hash,
		// different name (e.g. an unmoved freshly-merged feature).
		if hash, lookupErr := repo.ResolveRevision(plumbing.Revision(name)); lookupErr == nil && hash != nil && *hash == headHash {
			continue
		}
		unix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil {
			continue
		}
		candidates = append(candidates, candidate{name: name, tipUnix: unix})
	}

	if len(candidates) > 0 {
		// Pick the candidate with the most recent tip (closest ancestor).
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.tipUnix > best.tipUnix {
				best = c
			}
		}
		return best.name, nil
	}

	return fallbackScopeRef(repo)
}

// slowDetectScopeBaseRef is the pre-optimization fallback used only when the
// `git for-each-ref --merged` shell-out fails. It walks all refs and checks
// ancestry via repo.Log per ref (O(branches × commits)). Kept as a defense
// against environments where git CLI is unavailable but go-git can still
// resolve refs.
func slowDetectScopeBaseRef(ctx context.Context, repo *git.Repository, headHash plumbing.Hash, currentBranchShort string) (string, error) {
	refs, err := repo.References()
	if err != nil {
		return fallbackScopeRef(repo)
	}

	type candidate struct {
		name    string
		tipTime time.Time
	}
	var candidates []candidate

	_ = refs.ForEach(func(ref *plumbing.Reference) error { //nolint:errcheck // best-effort search
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !ref.Name().IsBranch() && !ref.Name().IsRemote() {
			return nil
		}
		if ref.Name().Short() == currentBranchShort {
			return nil
		}
		tipHash := ref.Hash()
		if tipHash == headHash {
			return nil
		}
		isAnc, ancErr := isAncestorOf(ctx, repo, tipHash, headHash)
		if ancErr != nil {
			return nil //nolint:nilerr // best-effort: skip unresolvable refs
		}
		if !isAnc {
			return nil
		}
		commit, cErr := repo.CommitObject(tipHash)
		if cErr != nil {
			return nil //nolint:nilerr // best-effort: skip refs with no commit object
		}
		candidates = append(candidates, candidate{
			name:    ref.Name().Short(),
			tipTime: commit.Committer.When,
		})
		return nil
	})
	if ctx.Err() != nil {
		return "", ctx.Err() //nolint:wrapcheck // propagate context cancellation
	}
	if len(candidates) > 0 {
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.tipTime.After(best.tipTime) {
				best = c
			}
		}
		return best.name, nil
	}
	return fallbackScopeRef(repo)
}

// repoWorktreePath returns the working-tree path for repo, or an error if the
// repo is bare or its worktree can't be resolved. detectScopeBaseRef needs
// this to invoke `git for-each-ref` with the right working directory.
func repoWorktreePath(repo *git.Repository) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("resolve worktree: %w", err)
	}
	return wt.Filesystem.Root(), nil
}

// fallbackScopeRef returns the first existing ref from the fallback chain:
// origin/HEAD → origin/main → origin/master → main → master.
// Returns an error if none exist.
func fallbackScopeRef(repo *git.Repository) (string, error) {
	chain := []string{"origin/HEAD", "origin/main", "origin/master", "main", "master"}
	for _, name := range chain {
		if refExists(repo, name) {
			return name, nil
		}
	}
	return "", errors.New("no suitable ancestor branch found; configure a base ref explicitly")
}

// refExists reports whether a ref with the given short name exists in repo.
func refExists(repo *git.Repository, shortName string) bool {
	refs, err := repo.References()
	if err != nil {
		return false
	}
	found := false
	_ = refs.ForEach(func(ref *plumbing.Reference) error { //nolint:errcheck // best-effort search
		if ref.Name().Short() == shortName {
			found = true
			return storer.ErrStop
		}
		return nil
	})
	return found
}

// isAncestorOf checks if candidate is an ancestor of (or equal to) target
// by walking the commit graph from target backwards.
func isAncestorOf(ctx context.Context, repo *git.Repository, candidate, target plumbing.Hash) (bool, error) {
	if candidate == target {
		return true, nil
	}

	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false, fmt.Errorf("log from target: %w", err)
	}
	defer iter.Close()

	found := false
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // storer.ErrStop is expected
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.Hash == candidate {
			found = true
			return storer.ErrStop
		}
		return nil
	})
	// Context cancellation: surface. storer.ErrStop or log exhaustion: ignore.
	if ctx.Err() != nil {
		return false, ctx.Err() //nolint:wrapcheck // propagate context cancellation
	}
	return found, nil
}

// currentBranchName returns the short branch name for the current HEAD,
// or "" for detached HEAD.
func currentBranchName(repo *git.Repository) string {
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	if !head.Name().IsBranch() {
		return "" // detached HEAD
	}
	return head.Name().Short()
}

// countCommits returns the number of commits in <baseRef>..HEAD
// (commits on the current branch not in baseRef).
func countCommits(ctx context.Context, repoRoot, baseRef string) (int, error) {
	out, err := runGit(ctx, repoRoot, "rev-list", "--count", baseRef+"..HEAD")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse commit count: %w", err)
	}
	return n, nil
}

// countFilesChanged returns the number of unique files changed in <baseRef>..HEAD.
func countFilesChanged(ctx context.Context, repoRoot, baseRef string) (int, error) {
	out, err := runGit(ctx, repoRoot, "diff", "--name-only", baseRef+"..HEAD")
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return 0, nil
	}
	return len(strings.Split(trimmed, "\n")), nil
}

// countUncommitted returns the number of lines in `git status --porcelain`
// (each line corresponds to one changed or untracked file).
func countUncommitted(ctx context.Context, repoRoot string) (int, error) {
	out, err := runGit(ctx, repoRoot, "status", "--porcelain")
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return 0, nil
	}
	return len(strings.Split(trimmed, "\n")), nil
}

// runGit runs `git <args>` in repoDir and returns stdout as a string.
// stderr is captured separately and surfaced in the error wrap on non-zero
// exit. Stdout and stderr are NOT combined — git emits warnings on stderr
// even on successful commands (shallow-clone notices, safe.directory
// advisories, etc.) and merging them would corrupt parsed output (e.g.,
// strconv.Atoi on the result of `rev-list --count` would fail).
func runGit(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Surface stderr so callers see why git rejected the command,
		// not just "exit status 128".
		stderrTxt := strings.TrimSpace(stderr.String())
		if stderrTxt != "" {
			return "", fmt.Errorf("git %s: %w (stderr: %s)", args[0], err, stderrTxt)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}
