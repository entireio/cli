package checkpoint

import (
	"context"

	"github.com/go-git/go-git/v6"
)

// NewCommittedReadStore returns a GitStore bound to the topology's read ref.
func NewCommittedReadStore(ctx context.Context, repo *git.Repository) *GitStore {
	return NewGitStoreWithRef(repo, ResolveCommittedRefs(ctx).Read)
}
