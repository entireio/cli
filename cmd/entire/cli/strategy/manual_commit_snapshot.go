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

// ErrPendingFileChanges reports that a session has uncommitted file changes.
// Its work is checkpointed by the next commit, so a snapshot is refused.
var ErrPendingFileChanges = errors.New("session has uncommitted file changes")

// CreateSnapshotCheckpoint writes a checkpoint from a session's current
// transcript and returns its ID. It exists for sessions that change no files —
// research, planning, review — which otherwise never get a checkpoint: turn end
// skips SaveStep when nothing changed, so session end finds no steps and writes
// nothing. A session with pending file changes is refused with
// ErrPendingFileChanges; the next commit checkpoints that work, and a
// snapshot would only duplicate it. "Pending" is what the
// transcript and file-touch hooks can see: an edit made through a shell
// command rather than an edit tool is only detected at turn end, so a snapshot
// in that same turn is not refused and overlaps the next commit's checkpoint.
//
// The checkpoint is written exactly as a condensation writes one — same
// extraction, redaction, and store write, so it is enqueued for push like any
// other — but it is not linked to a commit. A redaction failure is an error here
// rather than a dropped transcript (see condenseOpts.failOnRedactionError).
//
// It runs under the session's state lock, like every other condensation: the
// per-session redaction prefix cache is written in two steps, and an unlocked
// writer racing a commit's condensation can leave its entry pointing at the
// other writer's payload, corrupting every later checkpoint of the session.
// It is still a snapshot: the locked state is condensed in memory and never
// saved back (ErrMutationSkip), so the session's checkpoint window is
// untouched and repeated snapshots each cover the session from the same start.
// The crash-recovery reservation machinery therefore does not apply either — an
// interrupted write leaves at most an orphaned checkpoint, never a stuck session.
//
// Callers must configure redaction first (EnsureRedactionConfigured).
func (s *ManualCommitStrategy) CreateSnapshotCheckpoint(ctx context.Context, sessionID string) (id.CheckpointID, error) {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	repo, err := OpenRepository(ctx)
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	checkpointID, err := cpkg.GenerateCheckpointID(ctx)
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("generate checkpoint ID: %w", err)
	}

	var result *CondenseResult
	mutErr := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		// resolveFilesTouched, not state.FilesTouched alone: most agents report
		// no per-tool file events, so FilesTouched stays empty until SaveStep at
		// turn end, and an edit earlier in the current turn is only visible in
		// the live transcript. Paths outside the worktree (an agent's plan
		// files, say) are dropped there, so planning sessions are not refused.
		if len(s.resolveFilesTouched(ctx, state)) > 0 {
			return ErrPendingFileChanges
		}
		var condErr error
		result, condErr = s.CondenseSession(ctx, repo, checkpointID, state, nil, condenseOpts{failOnRedactionError: true})
		if condErr != nil {
			return fmt.Errorf("failed to create checkpoint: %w", condErr)
		}
		return ErrMutationSkip // a snapshot never saves the session back
	})
	if errors.Is(mutErr, ErrStateNotFound) {
		return id.EmptyCheckpointID, fmt.Errorf("session not found: %s", sessionID)
	}
	if mutErr != nil {
		return id.EmptyCheckpointID, mutErr
	}
	if result.Skipped {
		return id.EmptyCheckpointID, ErrNothingToCheckpoint
	}
	// No skill telemetry: the events are never marked persisted (state is not
	// saved), so emitting here would count them again on every snapshot.

	logging.Info(logCtx, "snapshot checkpoint created",
		slog.String("session_id", sessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
	)
	return result.CheckpointID, nil
}
