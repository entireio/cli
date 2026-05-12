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
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
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
// Used to build the banner before launching agents.
//
// baseOverride, if non-empty, bypasses the mainline auto-detection and is
// used as the scope base directly. This is the entry point for the
// `--base <ref>` command-line flag. The override is verified via
// `repo.ResolveRevision(plumbing.Revision(<ref>))` (matching the codebase
// pattern in explain.go:156) before being used; an unknown ref produces an
// error before any agents are spawned, so users learn about the typo
// immediately instead of after a 10-minute review run scoped against a
// default the flag was supposed to override.
func ComputeScopeStats(ctx context.Context, repo *git.Repository, baseOverride string) (ScopeStats, error) {
	repoRoot, err := repoWorktreePath(repo)
	if err != nil {
		return ScopeStats{}, fmt.Errorf("get repo root: %w", err)
	}

	var baseRef string
	if baseOverride != "" {
		// Validate via go-git rather than shelling out — matches the codebase
		// pattern in explain.go:156 (resolveCommitUnambiguous). ResolveRevision
		// handles branches, tags, abbreviated SHAs, and HEAD-relative refs, and
		// dereferences annotated tags to their target commit automatically.
		if _, vErr := repo.ResolveRevision(plumbing.Revision(baseOverride)); vErr != nil {
			return ScopeStats{}, fmt.Errorf("base ref %q does not resolve to a commit: %w", baseOverride, vErr)
		}
		baseRef = baseOverride
	} else {
		baseRef, err = detectScopeBaseRef(ctx, repo)
		if err != nil {
			return ScopeStats{}, fmt.Errorf("detect scope base ref: %w", err)
		}
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

// detectScopeBaseRef returns the mainline ref the review should be scoped
// against, walking the fallback chain origin/HEAD → origin/main →
// origin/master → main → master and returning the first that exists.
//
// A previous implementation tried to be clever: it picked the merged-into-HEAD
// branch with the most recent committerdate, on the theory that stacked PRs
// (feature B branched off feature A while A is still open) would benefit
// from reviewing against the immediate parent rather than mainline. In
// practice the heuristic routinely picked unrelated recently-merged feature
// branches — `git fetch` mirrors all of origin's branches by default, and
// any branch whose tip is newer than mainline AND merged into mainline
// (i.e., every recently-merged PR branch not yet deleted on origin) was a
// candidate. Reviews ended up scoped against random PR branches, dragging
// in 30+ commits of unrelated upstream work and producing reviews with
// nothing to do with the current branch.
//
// Stacked PR review is now served by the explicit `--base <ref>` flag at
// the command surface, not an inference. The default stays predictable
// (always mainline); the override is explicit when users actually want it.
func detectScopeBaseRef(_ context.Context, repo *git.Repository) (string, error) {
	return fallbackScopeRef(repo)
}

// repoWorktreePath returns the working-tree path for repo, or an error if the
// repo is bare or its worktree can't be resolved. ComputeScopeStats uses this
// as the cwd for the runGit invocations in countCommits / countFilesChanged /
// countUncommitted.
func repoWorktreePath(repo *git.Repository) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("resolve worktree: %w", err)
	}
	return wt.Filesystem().Root(), nil
}

// fallbackScopeRef returns the first existing ref from the fallback chain:
// origin/HEAD → origin/main → origin/master → main → master.
// Returns an error naming the tried refs if none exist.
func fallbackScopeRef(repo *git.Repository) (string, error) {
	chain := []string{"origin/HEAD", "origin/main", "origin/master", "main", "master"}
	for _, name := range chain {
		if refExists(repo, name) {
			return name, nil
		}
	}
	return "", fmt.Errorf(
		"no mainline ref found (tried %s); pass --base <ref> to scope the review explicitly, "+
			"or run `git fetch` to populate origin refs",
		strings.Join(chain, ", "))
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

// countFilesChanged returns the number of unique files changed on this
// branch since it diverged from baseRef. Uses three-dot diff syntax
// (`git diff base...HEAD`, equivalent to `git diff $(merge-base) HEAD`) so
// upstream-only changes on baseRef after the branch point are NOT counted
// as reversed deltas. Two-dot (`base..HEAD`) would over-count: every file
// modified on mainline since the branch was cut would appear as a
// "removed" change in the diff, inflating the banner's file count.
func countFilesChanged(ctx context.Context, repoRoot, baseRef string) (int, error) {
	out, err := runGit(ctx, repoRoot, "diff", "--name-only", baseRef+"...HEAD")
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
