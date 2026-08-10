package gitrepo

import (
	"context"
	"sync"

	"github.com/go-git/go-git/v6"
)

// Status is the single entry point for reading go-git worktree status; the
// forbidigo rule in .golangci.yaml keeps callers off worktree.Status directly.
//
// go-git's Worktree.Status() is expensive: it walks the whole worktree twice
// (once collecting .gitignore patterns, once diffing), and gitignore.ReadPatterns
// does not thread a parent directory's patterns into its recursive walk, so it
// only prunes an ignored directory when the matching pattern was declared by
// that directory's own parent .gitignore. A rule one level too deep leaves the
// whole subtree walked: a single call cost 5.25s in this repo before e2e's
// artifacts rule moved into e2e/.gitignore.

type statusCacheKey struct{}

type statusCache struct {
	mu       sync.Mutex
	statuses map[string]git.Status
}

// WithStatusCache returns a context that memoizes Status results.
//
// Install it only across a window that neither writes tracked files nor stages
// anything — otherwise later callers observe a stale status. Staging counts:
// .git/index lives inside .git/, but the index feeds the status diff, so an
// index write invalidates a cached result just as a worktree write does. Entire
// performs no index writes today (no SetIndex calls; the git subcommands on the
// hook paths are all index-read-only), which is what makes turn start a valid
// window. A hook that runs after the agent has edited files is not.
func WithStatusCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, statusCacheKey{}, &statusCache{
		statuses: make(map[string]git.Status),
	})
}

// Status returns the worktree status for repo, reusing a cached result when ctx
// carries a cache from WithStatusCache and the same worktree was already read.
//
// The returned map is shared with other callers holding the same cached ctx, so
// callers must treat it as read-only.
func Status(ctx context.Context, repo *git.Repository) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}

	cache, ok := ctx.Value(statusCacheKey{}).(*statusCache)
	if !ok {
		return worktree.Status() //nolint:wrapcheck,forbidigo // the sanctioned call site
	}

	// Key on the worktree root rather than the repository pointer: callers on
	// the same hook path open the repository independently.
	root := worktree.Filesystem().Root()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cached, hit := cache.statuses[root]; hit {
		return cached, nil
	}

	status, err := worktree.Status() //nolint:forbidigo // the sanctioned call site
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}
	cache.statuses[root] = status

	return status, nil
}
