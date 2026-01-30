package strategy

import (
	"fmt"
	"sync"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/session"
)

// ManualCommitStrategy implements the manual-commit strategy for session management.
// It stores checkpoints on shadow branches and condenses session logs to a
// permanent sessions branch when the user commits.
type ManualCommitStrategy struct {
	// stateStore manages session state files in .git/entire-sessions/
	stateStore *session.StateStore
	// stateStoreOnce ensures thread-safe lazy initialization
	stateStoreOnce sync.Once
	// stateStoreErr captures any error during initialization
	stateStoreErr error

	// checkpointStore manages checkpoint data in git
	checkpointStore *checkpoint.GitStore
	// checkpointStoreOnce ensures thread-safe lazy initialization
	checkpointStoreOnce sync.Once
	// checkpointStoreErr captures any error during initialization
	checkpointStoreErr error
}

// getStateStore returns the session state store, initializing it lazily if needed.
// Thread-safe via sync.Once.
func (s *ManualCommitStrategy) getStateStore() (*session.StateStore, error) {
	s.stateStoreOnce.Do(func() {
		store, err := session.NewStateStore()
		if err != nil {
			s.stateStoreErr = fmt.Errorf("failed to create state store: %w", err)
			return
		}
		s.stateStore = store
	})
	return s.stateStore, s.stateStoreErr
}

// getCheckpointStore returns the checkpoint store, initializing it lazily if needed.
// Thread-safe via sync.Once.
func (s *ManualCommitStrategy) getCheckpointStore() (*checkpoint.GitStore, error) {
	s.checkpointStoreOnce.Do(func() {
		repo, err := OpenRepository()
		if err != nil {
			s.checkpointStoreErr = fmt.Errorf("failed to open repository: %w", err)
			return
		}
		s.checkpointStore = checkpoint.NewGitStore(repo)
	})
	return s.checkpointStore, s.checkpointStoreErr
}

// sessionStateToStrategy converts session.State to strategy.SessionState.
func sessionStateToStrategy(state *session.State) *SessionState {
	if state == nil {
		return nil
	}
	result := &SessionState{
		SessionID:                   state.SessionID,
		BaseCommit:                  state.BaseCommit,
		WorktreePath:                state.WorktreePath,
		StartedAt:                   state.StartedAt,
		CheckpointCount:             state.CheckpointCount,
		CondensedTranscriptLines:    state.CondensedTranscriptLines,
		UntrackedFilesAtStart:       state.UntrackedFilesAtStart,
		FilesTouched:                state.FilesTouched,
		ConcurrentWarningShown:      state.ConcurrentWarningShown,
		LastCheckpointID:            state.LastCheckpointID,
		AgentType:                   state.AgentType,
		TokenUsage:                  state.TokenUsage,
		TranscriptLinesAtStart:      state.TranscriptLinesAtStart,
		TranscriptIdentifierAtStart: state.TranscriptIdentifierAtStart,
		TranscriptPath:              state.TranscriptPath,
		ShadowBranchSuffix:          state.ShadowBranchSuffix,
	}
	// Convert PromptAttributions
	for _, pa := range state.PromptAttributions {
		result.PromptAttributions = append(result.PromptAttributions, PromptAttribution{
			CheckpointNumber:  pa.CheckpointNumber,
			UserLinesAdded:    pa.UserLinesAdded,
			UserLinesRemoved:  pa.UserLinesRemoved,
			AgentLinesAdded:   pa.AgentLinesAdded,
			AgentLinesRemoved: pa.AgentLinesRemoved,
			UserAddedPerFile:  pa.UserAddedPerFile,
		})
	}
	// Convert PendingPromptAttribution
	if state.PendingPromptAttribution != nil {
		result.PendingPromptAttribution = &PromptAttribution{
			CheckpointNumber:  state.PendingPromptAttribution.CheckpointNumber,
			UserLinesAdded:    state.PendingPromptAttribution.UserLinesAdded,
			UserLinesRemoved:  state.PendingPromptAttribution.UserLinesRemoved,
			AgentLinesAdded:   state.PendingPromptAttribution.AgentLinesAdded,
			AgentLinesRemoved: state.PendingPromptAttribution.AgentLinesRemoved,
			UserAddedPerFile:  state.PendingPromptAttribution.UserAddedPerFile,
		}
	}
	return result
}

// sessionStateFromStrategy converts strategy.SessionState to session.State.
func sessionStateFromStrategy(state *SessionState) *session.State {
	if state == nil {
		return nil
	}
	result := &session.State{
		SessionID:                   state.SessionID,
		BaseCommit:                  state.BaseCommit,
		WorktreePath:                state.WorktreePath,
		StartedAt:                   state.StartedAt,
		CheckpointCount:             state.CheckpointCount,
		CondensedTranscriptLines:    state.CondensedTranscriptLines,
		UntrackedFilesAtStart:       state.UntrackedFilesAtStart,
		FilesTouched:                state.FilesTouched,
		ConcurrentWarningShown:      state.ConcurrentWarningShown,
		LastCheckpointID:            state.LastCheckpointID,
		AgentType:                   state.AgentType,
		TokenUsage:                  state.TokenUsage,
		TranscriptLinesAtStart:      state.TranscriptLinesAtStart,
		TranscriptIdentifierAtStart: state.TranscriptIdentifierAtStart,
		TranscriptPath:              state.TranscriptPath,
		ShadowBranchSuffix:          state.ShadowBranchSuffix,
	}
	// Convert PromptAttributions
	for _, pa := range state.PromptAttributions {
		result.PromptAttributions = append(result.PromptAttributions, session.PromptAttribution{
			CheckpointNumber:  pa.CheckpointNumber,
			UserLinesAdded:    pa.UserLinesAdded,
			UserLinesRemoved:  pa.UserLinesRemoved,
			AgentLinesAdded:   pa.AgentLinesAdded,
			AgentLinesRemoved: pa.AgentLinesRemoved,
			UserAddedPerFile:  pa.UserAddedPerFile,
		})
	}
	// Convert PendingPromptAttribution
	if state.PendingPromptAttribution != nil {
		result.PendingPromptAttribution = &session.PromptAttribution{
			CheckpointNumber:  state.PendingPromptAttribution.CheckpointNumber,
			UserLinesAdded:    state.PendingPromptAttribution.UserLinesAdded,
			UserLinesRemoved:  state.PendingPromptAttribution.UserLinesRemoved,
			AgentLinesAdded:   state.PendingPromptAttribution.AgentLinesAdded,
			AgentLinesRemoved: state.PendingPromptAttribution.AgentLinesRemoved,
			UserAddedPerFile:  state.PendingPromptAttribution.UserAddedPerFile,
		}
	}
	return result
}

// NewManualCommitStrategy creates a new manual-commit strategy instance.
//

func NewManualCommitStrategy() Strategy {
	return &ManualCommitStrategy{}
}

// NewShadowStrategy creates a new manual-commit strategy instance.
// This legacy constructor delegates to NewManualCommitStrategy.
//

func NewShadowStrategy() Strategy {
	return NewManualCommitStrategy()
}

// Name returns the strategy name.
func (s *ManualCommitStrategy) Name() string {
	return StrategyNameManualCommit
}

// Description returns the strategy description.
func (s *ManualCommitStrategy) Description() string {
	return "Manual commit checkpoints with session logs on entire/sessions"
}

// AllowsMainBranch returns true because manual-commit strategy only writes to shadow
// branches (entire/<hash>) and entire/sessions, never modifying the working branch's
// commit history.
func (s *ManualCommitStrategy) AllowsMainBranch() bool {
	return true
}

// ValidateRepository validates that the repository is suitable for this strategy.
func (s *ManualCommitStrategy) ValidateRepository() error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	_, err = repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to access worktree: %w", err)
	}

	return nil
}

// EnsureSetup ensures the strategy is properly set up.
func (s *ManualCommitStrategy) EnsureSetup() error {
	if err := EnsureEntireGitignore(); err != nil {
		return err
	}

	// Ensure the entire/sessions orphan branch exists for permanent session storage
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	if err := EnsureMetadataBranch(repo); err != nil {
		return fmt.Errorf("failed to ensure metadata branch: %w", err)
	}

	// Install generic hooks (they delegate to strategy at runtime)
	if !IsGitHookInstalled() {
		_, err := InstallGitHook(true)
		return err
	}
	return nil
}

// ListOrphanedItems returns orphaned items created by the manual-commit strategy.
// This includes:
//   - Shadow branches that weren't auto-cleaned during commit condensation
//   - Session state files with no corresponding checkpoints or shadow branches
func (s *ManualCommitStrategy) ListOrphanedItems() ([]CleanupItem, error) {
	var items []CleanupItem

	// Shadow branches (should have been auto-cleaned after condensation)
	branches, err := ListShadowBranches()
	if err != nil {
		return nil, err
	}
	for _, branch := range branches {
		items = append(items, CleanupItem{
			Type:   CleanupTypeShadowBranch,
			ID:     branch,
			Reason: "shadow branch (should have been auto-cleaned)",
		})
	}

	// Orphaned session states are detected by ListOrphanedSessionStates
	// which is strategy-agnostic (checks both shadow branches and checkpoints)

	return items, nil
}

//nolint:gochecknoinits // Standard pattern for strategy registration
func init() {
	// Register manual-commit as the primary strategy name
	Register(StrategyNameManualCommit, NewManualCommitStrategy)
}
