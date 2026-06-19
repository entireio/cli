package checkpoint

import (
	"context"
	"fmt"
)

// Write dispatches a committed write request to the matching git operation.
// The request types and the Writer interface are defined in the api/checkpoint
// contract (re-exported here via aliases). Unknown request types are a
// programmer error, surfaced rather than ignored.
func (s *GitStore) Write(ctx context.Context, req WriteRequest) error {
	switch r := req.(type) {
	case WriteSession:
		return s.WriteCommitted(ctx, WriteCommittedOptions(r))
	case BackfillTranscript:
		return s.UpdateCommitted(ctx, UpdateCommittedOptions(r))
	case BackfillSummary:
		return s.UpdateSummary(ctx, r.CheckpointID, r.Summary)
	case BackfillAttribution:
		return s.UpdateCheckpointSummary(ctx, r.CheckpointID, r.Attribution)
	default:
		return fmt.Errorf("checkpoint: unsupported write request %T", req)
	}
}
