package gitrepo

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/go-git/go-git/v6"
)

// Status is the single entry point for reading go-git worktree status; the
// forbidigo rule in .golangci.yaml enforces this and names this signature, so
// the context parameter is part of that contract and is where cancellation or a
// perf span would attach.
//
// Worktree.Status() walks the worktree, so its cost scales with working-set size
// rather than with the size of the change being inspected — it is the most
// expensive git read on the hook paths. Avoid calling it more than once per hook.
func Status(_ context.Context, repo *git.Repository) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}

	status, err := worktree.Status() //nolint:forbidigo // the sanctioned call site
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}
	filterNestedCheckouts(worktree.Filesystem().Root(), status)

	return status, nil
}

// filterNestedCheckouts removes untracked entries that live inside a nested
// git checkout: a directory under the worktree root containing a .git entry
// (a directory for full clones, a file for linked worktrees). git treats such
// a directory as a repository boundary and never descends into it; go-git's
// status walk has no such check, so untracked files from unrelated checkouts
// (agent worktrees, vendored clones) would otherwise be reported as if they
// were this repository's files. Tracked entries are kept regardless of
// location, matching git, which always reports index entries.
//
// Deliberately quieter than git in one respect: git reports the boundary
// itself as a single untracked entry ("?? vendor/"), while this filter drops
// it entirely. Consumers treat every status entry as a file to record, so a
// synthetic directory entry would be junk of the same kind this filter
// removes. Do not "fix" the missing boundary entry.
func filterNestedCheckouts(root string, status git.Status) {
	containsGit := make(map[string]bool)
	for relPath, fileStatus := range status {
		if fileStatus.Worktree != git.Untracked {
			continue
		}
		if insideNestedCheckout(root, relPath, containsGit) {
			delete(status, relPath)
		}
	}
}

// insideNestedCheckout reports whether any ancestor directory of relPath
// (a forward-slash path relative to root, as go-git status keys are) contains
// a .git entry. The walk stops before the root itself, so the host repo's own
// .git never counts. Lstat rather than Stat so a worktree's .git file counts
// without following it. containsGit memoizes per-directory answers across one
// status result, where thousands of paths can share a few ancestor directories.
func insideNestedCheckout(root, relPath string, containsGit map[string]bool) bool {
	for dir := path.Dir(relPath); dir != "." && dir != "/"; dir = path.Dir(dir) {
		nested, seen := containsGit[dir]
		if !seen {
			_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir), ".git"))
			nested = err == nil
			containsGit[dir] = nested
		}
		if nested {
			return true
		}
	}
	return false
}
