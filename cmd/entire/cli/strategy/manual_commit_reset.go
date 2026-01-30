package strategy

import (
	"fmt"
	"os"

	cpkg "entire.io/cli/cmd/entire/cli/checkpoint"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v5/plumbing"
)

// isAccessibleMode returns true if accessibility mode should be enabled.
// This checks the ACCESSIBLE environment variable.
func isAccessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

// Reset deletes shadow branches and session state for the current HEAD.
// This allows starting fresh without existing checkpoints.
// Handles both legacy (entire/<hash>) and suffixed (entire/<hash>-N) branch formats.
func (s *ManualCommitStrategy) Reset(force bool) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get current HEAD
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}
	headHash := head.Hash().String()

	// Find all sessions for the current HEAD commit
	sessions, err := s.findSessionsForCommit(headHash)
	if err != nil {
		// Log but continue - we'll still try to find branches directly
		fmt.Fprintf(os.Stderr, "Warning: failed to find sessions: %v\n", err)
	}

	// Collect all shadow branches to delete (both from sessions and legacy)
	branchesToDelete := make(map[string]plumbing.ReferenceName)

	// Add branches from sessions (using suffixed format)
	for _, state := range sessions {
		var branchName string
		if state.ShadowBranchSuffix > 0 {
			branchName = cpkg.ShadowBranchNameForCommitWithSuffix(state.BaseCommit, state.ShadowBranchSuffix)
		} else {
			branchName = getShadowBranchNameForCommit(state.BaseCommit)
		}
		refName := plumbing.NewBranchReferenceName(branchName)
		if _, err := repo.Reference(refName, true); err == nil {
			branchesToDelete[branchName] = refName
		}
	}

	// Also check for legacy branch (backward compatibility)
	legacyBranchName := getShadowBranchNameForCommit(headHash)
	legacyRefName := plumbing.NewBranchReferenceName(legacyBranchName)
	if _, err := repo.Reference(legacyRefName, true); err == nil {
		branchesToDelete[legacyBranchName] = legacyRefName
	}

	// If no branches to delete and no sessions, nothing to reset
	if len(branchesToDelete) == 0 && len(sessions) == 0 {
		fmt.Fprintf(os.Stderr, "No shadow branches found for current HEAD\n")
		return nil
	}

	// Build description for confirmation
	branchList := ""
	for branchName := range branchesToDelete {
		if branchList != "" {
			branchList += ", "
		}
		branchList += branchName
	}
	if branchList == "" {
		branchList = "(no branches, only session states)"
	}

	// Confirm before deleting
	if !force {
		confirmed := false
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Delete shadow branches?").
					Description(fmt.Sprintf("This will delete %s and all associated session state.\nThis action cannot be undone.", branchList)).
					Affirmative("Delete").
					Negative("Cancel").
					Value(&confirmed),
			),
		)
		if isAccessibleMode() {
			form = form.WithAccessible(true)
		}
		if err := form.Run(); err != nil {
			return fmt.Errorf("confirmation failed: %w", err)
		}
		if !confirmed {
			fmt.Fprintf(os.Stderr, "Cancelled\n")
			return nil
		}
	}

	// Clear all session states
	for _, state := range sessions {
		if err := s.clearSessionState(state.SessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to clear session state for %s: %v\n", state.SessionID, err)
		} else {
			fmt.Fprintf(os.Stderr, "Cleared session state for %s\n", state.SessionID)
		}
	}

	// Delete all shadow branches
	for branchName, refName := range branchesToDelete {
		if err := repo.Storer.RemoveReference(refName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to delete shadow branch %s: %v\n", branchName, err)
		} else {
			fmt.Fprintf(os.Stderr, "Deleted shadow branch %s\n", branchName)
		}
	}

	return nil
}
