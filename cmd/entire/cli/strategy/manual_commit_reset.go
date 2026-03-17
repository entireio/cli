package strategy

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6/plumbing"
)

// isAccessibleMode returns true if accessibility mode should be enabled.
// This checks the ACCESSIBLE environment variable.
func isAccessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

// Reset deletes the shadow branch and session state for the current HEAD.
// This allows starting fresh without existing checkpoints.
func (s *ManualCommitStrategy) Reset(ctx context.Context, w, errW io.Writer) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get current HEAD
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Get current worktree ID for shadow branch naming
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to get worktree path: %w", err)
	}
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		return fmt.Errorf("failed to get worktree ID: %w", err)
	}

	// Get shadow branch name for current HEAD
	shadowBranchName := getShadowBranchNameForCommit(head.Hash().String(), worktreeID)

	// Check if shadow branch exists
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	_, err = repo.Reference(refName, true)
	hasShadowBranch := err == nil

	// Find sessions for this commit
	sessions, err := s.findSessionsForCommit(ctx, head.Hash().String())
	if err != nil {
		sessions = nil // Ignore error, treat as no sessions
	}

	// If nothing to reset, return early
	if !hasShadowBranch && len(sessions) == 0 {
		fmt.Fprintf(w, "Nothing to reset for %s\n", shadowBranchName)
		return nil
	}

	// Clear all sessions for this commit
	clearedSessions := make([]string, 0)
	for _, state := range sessions {
		if err := s.clearSessionState(ctx, state.SessionID); err != nil {
			fmt.Fprintf(errW, "Warning: failed to clear session state for %s: %v\n", state.SessionID, err)
		} else {
			clearedSessions = append(clearedSessions, state.SessionID)
		}
	}

	// Report cleared session states with session IDs
	if len(clearedSessions) > 0 {
		for _, sessionID := range clearedSessions {
			fmt.Fprintf(w, "✓ Cleared session state for %s\n", sessionID)
		}
	}

	// Delete the shadow branch if it exists
	if hasShadowBranch {
		if err := DeleteBranchCLI(ctx, shadowBranchName); err != nil {
			return fmt.Errorf("failed to delete shadow branch: %w", err)
		}
		fmt.Fprintf(w, "✓ Deleted shadow branch %s\n", shadowBranchName)
	}

	return nil
}

// ResetSession clears a single session's state and removes the shadow branch
// if no other sessions reference it. File changes remain in the working directory.
func (s *ManualCommitStrategy) ResetSession(ctx context.Context, w, errW io.Writer, sessionID string) error {
	// Load the session state
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Clear the session state file
	if err := s.clearSessionState(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to clear session state: %w", err)
	}
	fmt.Fprintf(w, "✓ Cleared session state for %s\n", sessionID)

	// Determine the shadow branch for this session
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

	// Open repository
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Clean up shadow branch if no other sessions need it
	if err := s.cleanupShadowBranchIfUnused(ctx, repo, shadowBranchName, sessionID); err != nil {
		fmt.Fprintf(errW, "Warning: failed to clean up shadow branch %s: %v\n", shadowBranchName, err)
	} else {
		// Check if it was actually deleted via git CLI (go-git's cache
		// may be stale after CLI-based deletion with packed refs)
		if err := branchExistsCLI(ctx, shadowBranchName); err != nil {
			fmt.Fprintf(w, "✓ Deleted shadow branch %s\n", shadowBranchName)
		}
	}

	return nil
}
