package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// migrateShadowBranchIfNeeded checks if HEAD has changed since the session started
// and either reconciles or migrates the shadow branch accordingly.
//
// Reconcile path: if HEAD carries this session's LastCheckpointID as an
// Entire-Checkpoint trailer (e.g. after git reset --hard to a condensed commit),
// both BaseCommit and AttributionBaseCommit are updated to HEAD. The old shadow
// branch is intentionally left untouched to preserve rewind data.
//
// Migrate path: for all other HEAD changes (pull, rebase, tool-call commits),
// the shadow branch is renamed to the new base and only BaseCommit is updated.
// AttributionBaseCommit stays pinned for correct attribution.
//
// Returns (changed, reconciled, err):
//   - changed: true if either path fired, false for no-op
//   - reconciled: true only for the reconcile path; callers use this to know
//     that attribution must be recomputed against the new base since the old
//     base has been discarded (reconcile = reset-to-known-checkpoint)
func (s *ManualCommitStrategy) migrateShadowBranchIfNeeded(ctx context.Context, repo *git.Repository, state *SessionState) (bool, bool, error) {
	if state == nil || state.BaseCommit == "" {
		return false, false, nil
	}

	head, err := repo.Head()
	if err != nil {
		return false, false, fmt.Errorf("failed to get HEAD: %w", err)
	}

	currentHead := head.Hash().String()
	if state.BaseCommit == currentHead {
		return false, false, nil // No migration needed
	}

	// Reconcile path: if HEAD sits on the exact commit carrying this session's
	// LastCheckpointID, the user has reset back to the last condensed
	// checkpoint. Update both BaseCommit and AttributionBaseCommit to HEAD.
	// Deliberately do NOT rename or touch the old shadow branch — it
	// preserves rewind data from the discarded segment of history.
	//
	// The SHA guard (currentHead == LastCheckpointCommitHash) distinguishes a
	// true reset from a cherry-pick/rebase that merely preserved the trailer.
	// Cherry-picking a checkpoint commit creates a new SHA with the same
	// message; firing reconcile in that case would drop the pinned
	// AttributionBaseCommit and corrupt attribution for uncondensed work.
	// Legacy state files without LastCheckpointCommitHash fall back to
	// trailer-only matching for backward compatibility.
	if !state.LastCheckpointID.IsEmpty() {
		shaMismatch := state.LastCheckpointCommitHash != "" && state.LastCheckpointCommitHash != currentHead
		if shaMismatch {
			// Trailer may still be present via cherry-pick/rebase, but the
			// SHA doesn't match what we condensed into — fall through to migrate.
			logging.Debug(logging.WithComponent(ctx, "migration"), "reconcile skipped: HEAD SHA does not match LastCheckpointCommitHash",
				slog.String("head", currentHead[:7]),
				slog.String("expected", state.LastCheckpointCommitHash[:7]))
		} else {
			headCommit, commitErr := repo.CommitObject(head.Hash())
			if commitErr == nil {
				for _, cpID := range trailers.ParseAllCheckpoints(headCommit.Message) {
					if cpID.String() == state.LastCheckpointID.String() {
						state.BaseCommit = currentHead
						state.RealignAttributionBase(currentHead)
						logging.Info(logging.WithComponent(ctx, "migration"), "reconciled session to known checkpoint on HEAD",
							slog.String("checkpoint_id", state.LastCheckpointID.String()),
							slog.String("new_base", currentHead[:7]))
						return true, true, nil
					}
				}
			} else {
				logging.Warn(logging.WithComponent(ctx, "migration"), "could not load HEAD commit for reconcile check, falling through to migrate",
					slog.String("head", currentHead[:7]),
					slog.String("error", commitErr.Error()))
			}
		}
		// If SHA guard blocked, CommitObject failed, or no trailer matched,
		// fall through to the existing migrate path below.
	}

	changed, err := s.migrateShadowBranchToBaseCommit(ctx, repo, state, currentHead)
	return changed, false, err
}

// migrateShadowBranchToBaseCommit moves the current session's shadow branch to a
// new base commit-derived name and updates state.BaseCommit. It returns whether
// an existing shadow branch ref had to be migrated.
func (s *ManualCommitStrategy) migrateShadowBranchToBaseCommit(ctx context.Context, repo *git.Repository, state *SessionState, newBaseCommit string) (bool, error) {
	if state == nil || state.BaseCommit == "" || newBaseCommit == "" {
		return false, nil
	}
	if state.BaseCommit == newBaseCommit {
		return false, nil
	}

	// Base commit changed - check if old shadow branch exists and migrate it
	oldShadowBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	newShadowBranch := checkpoint.ShadowBranchNameForCommit(newBaseCommit, state.WorktreeID)

	// Guard against hash prefix collision: if both commits produce the same
	// shadow branch name (same 7-char prefix), just update state - no ref rename needed
	if oldShadowBranch == newShadowBranch {
		state.BaseCommit = newBaseCommit
		return true, nil
	}

	oldRefName := plumbing.NewBranchReferenceName(oldShadowBranch)
	oldRef, err := repo.Reference(oldRefName, true)
	if err != nil {
		// Old shadow branch doesn't exist - just update state.BaseCommit
		// This can happen if this is the first checkpoint after HEAD changed
		state.BaseCommit = newBaseCommit
		logging.Info(logging.WithComponent(ctx, "migration"), "updated session base commit",
			slog.String("new_base", newBaseCommit[:7]))
		return true, nil //nolint:nilerr // err is "reference not found" which is fine - just need to update state
	}

	// Old shadow branch exists - move it to new base commit
	newRefName := plumbing.NewBranchReferenceName(newShadowBranch)

	// Create new reference pointing to same commit as old shadow branch
	newRef := plumbing.NewHashReference(newRefName, oldRef.Hash())
	if err := repo.Storer.SetReference(newRef); err != nil {
		return false, fmt.Errorf("failed to create new shadow branch %s: %w", newShadowBranch, err)
	}

	// Delete old reference via CLI (go-git v5's RemoveReference doesn't persist with packed refs/worktrees)
	logCtx := logging.WithComponent(ctx, "migration")
	if err := DeleteBranchCLI(ctx, oldShadowBranch); err != nil {
		// Non-fatal: log but continue - the important thing is the new branch exists
		logging.Warn(logCtx, "failed to remove old shadow branch",
			slog.String("shadow_branch", oldShadowBranch),
			slog.String("error", err.Error()))
	}

	logging.Info(logCtx, "moved shadow branch (HEAD changed during session)",
		slog.String("from", oldShadowBranch),
		slog.String("to", newShadowBranch))

	// Update state with new base commit.
	// NOTE: AttributionBaseCommit is intentionally NOT updated here. Migration
	// renames the shadow branch but its checkpoint trees are still relative to
	// the original base. Attribution must diff from that original base to
	// correctly measure agent work captured in those checkpoints.
	state.BaseCommit = newBaseCommit
	return true, nil
}
