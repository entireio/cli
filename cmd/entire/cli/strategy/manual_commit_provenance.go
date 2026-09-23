package strategy

import (
	"context"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
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

func (s *ManualCommitStrategy) condensationTarget(ctx context.Context, repo *git.Repository, ids []id.CheckpointID) (id.CheckpointID, bool) {
	if len(ids) <= 1 {
		return pickCondensationTarget(ids, nil)
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		// Without a store nothing can be told apart; keep the last trailer,
		// which is where prepare stamps.
		return ids[len(ids)-1], true
	}
	return pickCondensationTarget(ids, func(cpID id.CheckpointID) bool { return checkpointExists(ctx, store, cpID) })
}

// checkpointBelongsElsewhere reports whether checkpointID already holds a
// checkpoint that this session did not stamp for this commit: neither the one
// it is amending (LastCheckpointID) nor its pending reservation. Writing there
// would fold new work into another commit's record.
func (s *ManualCommitStrategy) checkpointBelongsElsewhere(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID, state *SessionState) bool {
	if checkpointID == state.LastCheckpointID || checkpointID == state.PendingCondensationID() {
		return false
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return false
	}
	return checkpointExists(ctx, store, checkpointID)
}

func checkpointExists(ctx context.Context, store checkpoint.PersistentStore, checkpointID id.CheckpointID) bool {
	summary, err := store.Read(ctx, checkpointID)
	return err == nil && summary != nil
}
