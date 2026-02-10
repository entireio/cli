package strategy

import (
	"fmt"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
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

// NewManualCommitStrategy creates a new manual-commit strategy instance.
//

func NewManualCommitStrategy() Strategy { //nolint:ireturn // already present in codebase
	return &ManualCommitStrategy{}
}

// NewShadowStrategy creates a new manual-commit strategy instance.
// This legacy constructor delegates to NewManualCommitStrategy.
//

func NewShadowStrategy() Strategy { //nolint:ireturn // already present in codebase
	return NewManualCommitStrategy()
}

// Name returns the strategy name.
func (s *ManualCommitStrategy) Name() string {
	return StrategyNameManualCommit
}

// Description returns the strategy description.
func (s *ManualCommitStrategy) Description() string {
	return "Manual commit checkpoints with session logs on entire/checkpoints/v1"
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

	// Ensure the entire/checkpoints/v1 orphan branch exists for permanent session storage
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
