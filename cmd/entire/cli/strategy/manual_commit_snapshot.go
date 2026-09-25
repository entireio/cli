package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ErrNothingToCheckpoint reports that a session has no transcript or files to
// write, so no checkpoint was created.
var ErrNothingToCheckpoint = errors.New("session has nothing to checkpoint")

// CreateSnapshotCheckpoint writes a checkpoint from a session's current
// transcript and returns its ID. The checkpoint is written exactly as a
// condensation writes one — same extraction, redaction, and store write, so it
// is enqueued for push like any other — but it is not linked to a commit, and
// so carries no code attribution (see condenseOpts.noCommitAttribution).
//
// It is a snapshot: the session state is loaded fresh, condensed from that
// in-memory copy, and never saved back. The session's checkpoint window
// (StepCount, CheckpointTranscriptStart, FilesTouched, any pending
// condensation reservation) is untouched, so the next commit condenses the
// same range again and the two checkpoints overlap. That is deliberate: the
// caller is typically the agent itself mid-turn, and mutating a live session's
// bookkeeping from outside its hooks would race them. It also means the
// crash-recovery reservation machinery does not apply — an interrupted write
// leaves at most an orphaned checkpoint, never a stuck session.
//
// Callers must configure redaction first (EnsureRedactionConfigured).
func (s *ManualCommitStrategy) CreateSnapshotCheckpoint(ctx context.Context, sessionID string) (id.CheckpointID, error) {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return id.EmptyCheckpointID, err
	}
	if state == nil {
		return id.EmptyCheckpointID, fmt.Errorf("session not found: %s", sessionID)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	checkpointID, err := cpkg.GenerateCheckpointID(ctx)
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("generate checkpoint ID: %w", err)
	}

	result, err := s.CondenseSession(ctx, repo, checkpointID, state, nil, condenseOpts{noCommitAttribution: true})
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("failed to create checkpoint: %w", err)
	}
	if result.Skipped {
		return id.EmptyCheckpointID, ErrNothingToCheckpoint
	}
	// No skill telemetry: the events are not marked persisted (state is never
	// saved), so the next commit's condensation emits them — emitting here too
	// would count each one twice.

	logging.Info(logCtx, "snapshot checkpoint created",
		slog.String("session_id", sessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
	)
	return result.CheckpointID, nil
}
