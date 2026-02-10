package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Shadow strategy session state methods.
// Uses session.StateStore for persistence.

// loadSessionState loads session state using the StateStore.
func (s *ManualCommitStrategy) loadSessionState(sessionID string) (*SessionState, error) {
	store, err := s.getStateStore()
	if err != nil {
		return nil, err
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session state: %w", err)
	}
	return state, nil
}

// saveSessionState saves session state using the StateStore.
func (s *ManualCommitStrategy) saveSessionState(state *SessionState) error {
	store, err := s.getStateStore()
	if err != nil {
		return err
	}
	if err := store.Save(context.Background(), state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	return nil
}

// clearSessionState clears session state using the StateStore.
func (s *ManualCommitStrategy) clearSessionState(sessionID string) error {
	store, err := s.getStateStore()
	if err != nil {
		return err
	}
	if err := store.Clear(context.Background(), sessionID); err != nil {
		return fmt.Errorf("failed to clear session state: %w", err)
	}
	return nil
}

// listAllSessionStates returns all active session states.
// It filters out orphaned sessions whose shadow branch no longer exists.
func (s *ManualCommitStrategy) listAllSessionStates() ([]*SessionState, error) {
	store, err := s.getStateStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get state store: %w", err)
	}

	sessionStates, err := store.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	if len(sessionStates) == 0 {
		return nil, nil
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	var states []*SessionState
	for _, sessionState := range sessionStates {
		state := sessionState

		// Skip and cleanup orphaned sessions whose shadow branch no longer exists.
		// Keep active sessions (shadow branch may not be created yet) and sessions
		// with LastCheckpointID (needed for checkpoint ID reuse on subsequent commits).
		// Clean up everything else: stale pre-state-machine sessions (empty phase),
		// IDLE/ENDED sessions that were never condensed, etc.
		shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		refName := plumbing.NewBranchReferenceName(shadowBranch)
		if _, err := repo.Reference(refName, true); err != nil {
			if !state.Phase.IsActive() && state.LastCheckpointID.IsEmpty() {
				//nolint:errcheck,gosec // G104: Cleanup is best-effort, shouldn't fail the list operation
				store.Clear(context.Background(), state.SessionID)
				continue
			}
		}

		states = append(states, state)
	}
	return states, nil
}

// findSessionsForWorktree finds all sessions for the given worktree path.
func (s *ManualCommitStrategy) findSessionsForWorktree(worktreePath string) ([]*SessionState, error) {
	allStates, err := s.listAllSessionStates()
	if err != nil {
		return nil, err
	}

	var matching []*SessionState
	for _, state := range allStates {
		if state.WorktreePath == worktreePath {
			matching = append(matching, state)
		}
	}
	return matching, nil
}

// findSessionsForCommit finds all sessions where base_commit matches the given SHA.
func (s *ManualCommitStrategy) findSessionsForCommit(baseCommitSHA string) ([]*SessionState, error) {
	allStates, err := s.listAllSessionStates()
	if err != nil {
		return nil, err
	}

	var matching []*SessionState
	for _, state := range allStates {
		if state.BaseCommit == baseCommitSHA {
			matching = append(matching, state)
		}
	}
	return matching, nil
}

// FindSessionsForCommit is the exported version of findSessionsForCommit.
// Used by the rewind reset command to find sessions to clean up.
func (s *ManualCommitStrategy) FindSessionsForCommit(baseCommitSHA string) ([]*SessionState, error) {
	return s.findSessionsForCommit(baseCommitSHA)
}

// ClearSessionState is the exported version of clearSessionState.
// Used by the rewind reset command to clean up session state files.
func (s *ManualCommitStrategy) ClearSessionState(sessionID string) error {
	return s.clearSessionState(sessionID)
}

// CountOtherActiveSessionsWithCheckpoints counts how many other active sessions
// from the SAME worktree (different from currentSessionID) have created checkpoints
// on the SAME base commit (current HEAD). This is used to show an informational message
// about concurrent sessions that will be included in the next commit.
// Returns 0, nil if no such sessions exist.
func (s *ManualCommitStrategy) CountOtherActiveSessionsWithCheckpoints(currentSessionID string) (int, error) {
	currentWorktree, err := GetWorktreePath()
	if err != nil {
		return 0, err
	}

	// Get current HEAD to compare with session base commits
	repo, err := OpenRepository()
	if err != nil {
		return 0, err
	}
	head, err := repo.Head()
	if err != nil {
		return 0, fmt.Errorf("failed to get HEAD: %w", err)
	}
	currentHead := head.Hash().String()

	allStates, err := s.listAllSessionStates()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, state := range allStates {
		// Only consider sessions from the same worktree with checkpoints
		// AND based on the same commit (current HEAD)
		// Sessions from different base commits are independent and shouldn't be counted
		if state.SessionID != currentSessionID &&
			state.WorktreePath == currentWorktree &&
			state.StepCount > 0 &&
			state.BaseCommit == currentHead {
			count++
		}
	}
	return count, nil
}

// initializeSession creates a new session state or updates a partial one.
// A partial state may exist if the concurrent session warning was shown.
// agentType is the human-readable name of the agent (e.g., "Claude Code").
// transcriptPath is the path to the live transcript file (for mid-session commit detection).
// userPrompt is the user's prompt text (stored truncated as FirstPrompt for display).
func (s *ManualCommitStrategy) initializeSession(repo *git.Repository, sessionID string, agentType agent.AgentType, transcriptPath string, userPrompt string) (*SessionState, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	worktreePath, err := GetWorktreePath()
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree path: %w", err)
	}

	// Get worktree ID for shadow branch naming
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree ID: %w", err)
	}

	// Capture untracked files at session start to preserve them during rewind
	untrackedFiles, err := collectUntrackedFiles()
	if err != nil {
		// Non-fatal: continue even if we can't collect untracked files
		untrackedFiles = nil
	}

	now := time.Now()
	headHash := head.Hash().String()
	state := &SessionState{
		SessionID:             sessionID,
		BaseCommit:            headHash,
		AttributionBaseCommit: headHash,
		WorktreePath:          worktreePath,
		WorktreeID:            worktreeID,
		StartedAt:             now,
		LastInteractionTime:   &now,
		StepCount:             0,
		UntrackedFilesAtStart: untrackedFiles,
		AgentType:             agentType,
		TranscriptPath:        transcriptPath,
		FirstPrompt:           truncatePromptForStorage(userPrompt),
	}

	if err := s.saveSessionState(state); err != nil {
		return nil, err
	}

	return state, nil
}

// getShadowBranchNameForCommit returns the shadow branch name for the given base commit and worktree ID.
// worktreeID should be empty for the main worktree or the internal git worktree name for linked worktrees.
func getShadowBranchNameForCommit(baseCommit, worktreeID string) string {
	return checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
}
