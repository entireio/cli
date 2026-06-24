package checkpoint

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// AuthorReader provides optional checkpoint author lookup. It stays in the
// implementation package: GetCheckpointAuthor is a git-log operation and Author
// is an implementation type, not part of the storage contract.
type AuthorReader interface {
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error)
}

// normalizeCheckpointSummary fills in the checkpoint metadata format version
// for summaries read back without one (older records predate the field).
func normalizeCheckpointSummary(summary *CheckpointSummary) *CheckpointSummary {
	if summary == nil {
		return nil
	}
	if summary.CheckpointVersion == "" {
		summary.CheckpointVersion = CheckpointVersionBranchV1
	}
	return summary
}
