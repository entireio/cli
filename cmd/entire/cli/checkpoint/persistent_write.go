package checkpoint

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

type persistentWriteBackend interface {
	writeBatchSessions(ctx context.Context, req BatchSessions) error
	backfillTranscript(ctx context.Context, opts UpdateOptions) error
	backfillSummary(ctx context.Context, req SessionSummary) error
	backfillAttribution(ctx context.Context, checkpointID id.CheckpointID, attribution *Attribution) error
}

// Write dispatches a persistent write request to the matching git operation.
// The request types and Writer interface are defined in the api/checkpoint
// contract (re-exported here via aliases). Unknown request types are a
// programmer error, surfaced rather than ignored.
func (s *GitStore) Write(ctx context.Context, req WriteRequest) error {
	return dispatchPersistentWrite(ctx, s, req)
}

func dispatchPersistentWrite(ctx context.Context, backend persistentWriteBackend, req WriteRequest) error {
	switch r := req.(type) {
	case Session:
		return writeSingleSessionViaBatch(ctx, backend, WriteOptions(r))
	case ReservedSession:
		return writeSingleSessionViaBatch(ctx, backend, WriteOptions(r))
	case BatchSessions:
		return backend.writeBatchSessions(ctx, r)
	case SessionTranscript:
		return backend.backfillTranscript(ctx, UpdateOptions(r))
	case SessionSummary:
		return backend.backfillSummary(ctx, r)
	case CheckpointAttribution:
		return backend.backfillAttribution(ctx, r.CheckpointID, r.Attribution)
	default:
		return fmt.Errorf("checkpoint: unsupported write request %T", req)
	}
}
