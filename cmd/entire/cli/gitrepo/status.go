package gitrepo

import (
	"context"

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

	return worktree.Status() //nolint:wrapcheck,forbidigo // the sanctioned call site
}
