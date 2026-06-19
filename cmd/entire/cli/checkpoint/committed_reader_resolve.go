package checkpoint

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// AuthorReader provides optional checkpoint author lookup. It stays in the
// implementation package because GetCheckpointAuthor is a git-log operation
// and Author is an implementation type, not part of the storage contract.
type AuthorReader interface {
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error)
}
