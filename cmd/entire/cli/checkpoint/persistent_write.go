package checkpoint

import (
	"context"
	"fmt"
)

// Write dispatches a persistent write request to the matching git operation.
// The request types and Writer interface are defined in the api/checkpoint
// contract (re-exported here via aliases). Unknown request types are a
// programmer error, surfaced rather than ignored.
func (s *GitStore) Write(ctx context.Context, req WriteRequest) error {
	switch r := req.(type) {
	case Session:
		return s.writeSession(ctx, WriteOptions(r))
	case SessionTranscript:
		return s.backfillTranscript(ctx, UpdateOptions(r))
	case SessionSummary:
		return s.backfillSummary(ctx, r.CheckpointID, r.Summary)
	case CheckpointAttribution:
		return s.backfillAttribution(ctx, r.CheckpointID, r.Attribution)
	default:
		return fmt.Errorf("checkpoint: unsupported write request %T", req)
	}
}
