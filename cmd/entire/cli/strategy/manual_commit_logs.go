package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// GetMetadataRef returns a reference to the metadata for the given checkpoint.
// For manual-commit strategy, returns the sharded path on refs.Primary.
func (s *ManualCommitStrategy) GetMetadataRef(ctx context.Context, cp Checkpoint) string {
	if cp.CheckpointID.IsEmpty() {
		return ""
	}
	refs := checkpoint.ResolveRefs(ctx)
	return refs.Primary.Short() + ":" + cp.CheckpointID.Path()
}

// GetSessionMetadataRef returns a reference to the most recent metadata commit for a session.
// For manual-commit strategy, metadata lives on refs.Primary.
func (s *ManualCommitStrategy) GetSessionMetadataRef(ctx context.Context, _ string) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}
	defer repo.Close()

	refs := checkpoint.ResolveRefs(ctx)
	ref, err := repo.Reference(refs.Primary, true)
	if err != nil {
		return ""
	}

	// The tip of Primary contains all condensed sessions; return a reference to
	// it (sessionID is not used because all sessions live on the same ref).
	return trailers.FormatSourceRef(refs.Primary.Short(), ref.Hash().String())
}

// GetCheckpointLog returns the session transcript for a specific checkpoint.
// For manual-commit strategy, metadata is stored at sharded paths on entire/checkpoints/v1 branch.
func (s *ManualCommitStrategy) GetCheckpointLog(ctx context.Context, checkpoint Checkpoint) ([]byte, error) { //nolint:unparam // []byte is used by callers; lint false positive from test-only usage
	if checkpoint.CheckpointID.IsEmpty() {
		return nil, ErrNoMetadata
	}
	return s.getCheckpointLog(ctx, checkpoint.CheckpointID)
}
