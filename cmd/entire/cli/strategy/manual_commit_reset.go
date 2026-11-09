package strategy

import (
	"context"
	"fmt"
	"io"
)

// Reset clears the session state of every session based on the current HEAD,
// and deletes the shadow branches older CLIs left behind (see
// ListRemovableLegacyShadowBranches). File changes remain in the working
// directory.
func (s *ManualCommitStrategy) Reset(ctx context.Context, w, errW io.Writer) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	sessions, err := s.findSessionsForCommit(ctx, head.Hash().String())
	if err != nil {
		sessions = nil // Ignore error, treat as no sessions
	}
	legacyBranches, err := ListRemovableLegacyShadowBranches(ctx)
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to list legacy shadow branches: %v\n", err)
		legacyBranches = nil
	}

	if len(sessions) == 0 && len(legacyBranches) == 0 {
		fmt.Fprintln(w, "Nothing to clean for current HEAD.")
		return nil
	}

	for _, state := range sessions {
		if err := withLockWaitNotice(state.SessionID, errW, SessionLockNoticeDelay, func() error {
			return s.clearSessionState(ctx, state.SessionID)
		}); err != nil {
			fmt.Fprintf(errW, "Warning: failed to clear session state for %s: %v\n", state.SessionID, err)
			continue
		}
		fmt.Fprintf(w, "✓ Cleared session state for %s\n", state.SessionID)
	}

	deleted, failed := DeleteLegacyShadowBranches(ctx, legacyBranches)
	for _, branch := range deleted {
		fmt.Fprintf(w, "✓ Deleted legacy shadow branch %s\n", branch)
	}
	for _, branch := range failed {
		fmt.Fprintf(errW, "Warning: failed to delete legacy shadow branch %s\n", branch)
	}
	return nil
}

// ListRemovableLegacyShadowBranches returns the local shadow branches older
// CLIs left behind that are safe to delete without listing them for a human
// first: the strict worktree-suffixed shape only (see
// autoDeletableLegacyShadowBranchPattern), sorted.
func ListRemovableLegacyShadowBranches(ctx context.Context) ([]string, error) {
	heads, err := listLegacyShadowBranchHeads(ctx, isAutoDeletableLegacyShadowBranch)
	if err != nil {
		return nil, err
	}
	return sortedBranchNames(heads), nil
}

// ResetSession clears a single session's state. File changes remain in the
// working directory.
func (s *ManualCommitStrategy) ResetSession(ctx context.Context, w, errW io.Writer, sessionID string) error {
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Clear the session state file. Reset is interactive and takes the same
	// unbounded gate doctor does, so it gets the same lock-wait notice.
	if err := withLockWaitNotice(sessionID, errW, SessionLockNoticeDelay, func() error {
		return s.clearSessionState(ctx, sessionID)
	}); err != nil {
		return fmt.Errorf("failed to clear session state: %w", err)
	}
	fmt.Fprintf(w, "✓ Cleared session state for %s\n", sessionID)
	return nil
}
