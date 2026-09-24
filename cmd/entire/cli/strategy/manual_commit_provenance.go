package strategy

import (
	"context"
	"log/slog"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// stampedTrailer returns the first checkpoint trailer in message that
// prepare-commit-msg stamped itself, skipping trailers inherited from
// squashed commits: those link existing checkpoints and never stand in for a
// fresh one.
func stampedTrailer(message string, inherited []id.CheckpointID) (id.CheckpointID, bool) {
	skip := make(map[id.CheckpointID]bool, len(inherited))
	for _, cpID := range inherited {
		skip[cpID] = true
	}
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		if !skip[cpID] {
			return cpID, true
		}
	}
	return id.EmptyCheckpointID, false
}

// pickCondensationTarget chooses the trailer a commit's new work condenses
// into. A lone trailer is it. Among several, trailers that already have a
// checkpoint are links; the stamped one is the last without a checkpoint,
// since prepare appends after the inherited ones. None means the commit only
// links existing work.
func pickCondensationTarget(ids []id.CheckpointID, exists func(id.CheckpointID) bool) (id.CheckpointID, bool) {
	if len(ids) == 1 {
		return ids[0], true
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if !exists(ids[i]) {
			return ids[i], true
		}
	}
	return id.EmptyCheckpointID, false
}

func pickCondensationTargetState(ids []id.CheckpointID, exists func(id.CheckpointID) bool) (target id.CheckpointID, preexisting, found bool) {
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false, false
	}
	if len(ids) == 1 {
		return ids[0], exists(ids[0]), true
	}
	target, found = pickCondensationTarget(ids, exists)
	if !found {
		return id.EmptyCheckpointID, false, false
	}
	// Recheck after selection: another session may have created the checkpoint
	// between the first store read and condensation.
	return target, exists(target), true
}

// condensationTarget picks the trailer this commit condenses into and reports
// whether that checkpoint already existed before this post-commit began. Read
// once per commit: sessions condensing into the same fresh checkpoint later in
// this hook must not look like writes into someone else's.
func (s *ManualCommitStrategy) condensationTarget(ctx context.Context, repo *git.Repository, ids []id.CheckpointID) (target id.CheckpointID, preexisting, found bool) {
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false, false
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		// Nothing can be told apart without a store; the last trailer is
		// where prepare stamps.
		return ids[len(ids)-1], false, true
	}
	exists := func(cpID id.CheckpointID) bool { return checkpointExists(ctx, store, cpID) }
	target, preexisting, ok := pickCondensationTargetState(ids, exists)
	if !ok {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "post-commit: every trailer links an existing checkpoint; nothing to condense",
			slog.Int("trailers", len(ids)))
	}
	return target, preexisting, ok
}

type preexistingTargetKey struct{}

// withPreexistingTarget marks a post-commit whose condensation target already
// had a checkpoint when the hook began.
func withPreexistingTarget(ctx context.Context) context.Context {
	return context.WithValue(ctx, preexistingTargetKey{}, true)
}

// stampedByAnotherCommit reports whether checkpointID is a preexisting
// checkpoint this session did not stamp for this commit: neither the one it is
// amending (LastCheckpointID) nor its pending reservation. Writing there would
// fold new work into another commit's record.
func stampedByAnotherCommit(ctx context.Context, checkpointID id.CheckpointID, state *SessionState) bool {
	if ctx.Value(preexistingTargetKey{}) != true {
		return false
	}
	return checkpointID != state.LastCheckpointID && checkpointID != state.PendingCondensationID()
}

func checkpointExists(ctx context.Context, store checkpoint.PersistentStore, checkpointID id.CheckpointID) bool {
	summary, err := store.Read(ctx, checkpointID)
	return err == nil && summary != nil
}
