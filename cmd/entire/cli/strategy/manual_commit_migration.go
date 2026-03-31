package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// migrateShadowBranchIfNeeded checks if HEAD has changed since the session started
// and migrates the shadow branch to the new base commit if needed.
//
// This handles the scenario where Claude performs a rebase, pull, or other git operation
// that changes HEAD mid-session (via a tool call), without a new prompt being submitted.
// Without this migration, checkpoints would be saved to an orphaned shadow branch.
//
// Returns true if migration occurred, false otherwise.
func (s *ManualCommitStrategy) migrateShadowBranchIfNeeded(ctx context.Context, repo *git.Repository, state *SessionState) (bool, error) {
	if state == nil || state.BaseCommit == "" {
		return false, nil
	}

	head, err := repo.Head()
	if err != nil {
		return false, fmt.Errorf("failed to get HEAD: %w", err)
	}

	currentHead := head.Hash().String()
	if state.BaseCommit == currentHead {
		return false, nil // No migration needed
	}

	// HEAD changed - check if old shadow branch exists and migrate it
	oldShadowBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	newShadowBranch := checkpoint.ShadowBranchNameForCommit(currentHead, state.WorktreeID)

	// Guard against hash prefix collision: if both commits produce the same
	// shadow branch name (same 7-char prefix), just update state - no ref rename needed
	if oldShadowBranch == newShadowBranch {
		state.BaseCommit = currentHead
		return true, nil
	}

	oldRefName := plumbing.NewBranchReferenceName(oldShadowBranch)
	oldRef, err := repo.Reference(oldRefName, true)
	if err != nil {
		// Old shadow branch doesn't exist - just update state.BaseCommit
		// This can happen if this is the first checkpoint after HEAD changed
		state.BaseCommit = currentHead
		logging.Info(logging.WithComponent(ctx, "migration"), "updated session base commit (HEAD changed during session)",
			slog.String("new_base", currentHead[:7]))
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
	state.BaseCommit = currentHead
	return true, nil
}

// migrateAndPersistIfNeeded checks for HEAD changes, migrates the shadow branch if needed,
// and persists the updated session state. Used by SaveStep and SaveTaskStep.
func (s *ManualCommitStrategy) migrateAndPersistIfNeeded(ctx context.Context, repo *git.Repository, state *SessionState) error {
	migrated, err := s.migrateShadowBranchIfNeeded(ctx, repo, state)
	if err != nil {
		return fmt.Errorf("failed to check/migrate shadow branch: %w", err)
	}
	if migrated {
		if err := s.saveSessionState(ctx, state); err != nil {
			return fmt.Errorf("failed to save session state after migration: %w", err)
		}
	}
	return nil
}
