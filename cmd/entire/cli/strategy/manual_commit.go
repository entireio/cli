package strategy

import (
	"context"
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

	// blobFetcher, when set, is passed to the checkpoint store to enable
	// on-demand blob fetching after treeless fetches. Set via SetBlobFetcher.
	blobFetcher checkpoint.BlobFetchFunc

	// v2CheckpointStore manages v2 checkpoint reads
	v2CheckpointStore     *checkpoint.V2GitStore
	v2CheckpointStoreOnce sync.Once
	v2CheckpointStoreErr  error
}

// getStateStore returns the session state store, initializing it lazily if needed.
// Thread-safe via sync.Once.
func (s *ManualCommitStrategy) getStateStore(_ context.Context) (*session.StateStore, error) {
	s.stateStoreOnce.Do(func() {
		store, err := session.NewStateStore(context.Background()) //nolint:contextcheck // sync.Once must use background context to avoid caching errors from a cancelled caller context
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
		repo, err := OpenRepository(context.Background())
		if err != nil {
			s.checkpointStoreErr = fmt.Errorf("failed to open repository: %w", err)
			return
		}
		WarnIfMetadataDisconnected()
		store := checkpoint.NewGitStore(repo)
		if s.blobFetcher != nil {
			store.SetBlobFetcher(s.blobFetcher)
		}
		s.checkpointStore = store
	})
	return s.checkpointStore, s.checkpointStoreErr
}

// getV2CheckpointStore returns the v2 checkpoint store, initializing it lazily.
// The context from the first call is used for initialization (settings loading, repo opening).
func (s *ManualCommitStrategy) getV2CheckpointStore(ctx context.Context) (*checkpoint.V2GitStore, error) {
	s.v2CheckpointStoreOnce.Do(func() {
		repo, err := OpenRepository(ctx)
		if err != nil {
			s.v2CheckpointStoreErr = fmt.Errorf("failed to open repository: %w", err)
			return
		}
		s.v2CheckpointStore = checkpoint.NewV2GitStore(repo, ResolveCheckpointURL(ctx, "origin"))
	})
	return s.v2CheckpointStore, s.v2CheckpointStoreErr
}

// NewManualCommitStrategy creates a new manual-commit strategy instance.
func NewManualCommitStrategy() *ManualCommitStrategy {
	return &ManualCommitStrategy{}
}

// SetBlobFetcher configures on-demand blob fetching for the checkpoint store.
// Must be called before the first checkpoint store access (e.g., before RestoreLogsOnly).
func (s *ManualCommitStrategy) SetBlobFetcher(f checkpoint.BlobFetchFunc) {
	s.blobFetcher = f
}

// HasBlobFetcher reports whether a blob fetcher is configured.
// Used in tests to verify the strategy is properly wired for treeless fetch support.
func (s *ManualCommitStrategy) HasBlobFetcher() bool {
	return s.blobFetcher != nil
}

// ValidateRepository validates that the repository is suitable for this strategy.
func (s *ManualCommitStrategy) ValidateRepository() error {
	repo, err := OpenRepository(context.Background())
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	_, err = repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to access worktree: %w", err)
	}

	return nil
}

// ListOrphanedItems returns orphaned items created by the manual-commit strategy.
// This includes:
//   - Shadow branches that weren't auto-cleaned during commit condensation
//   - Session state files with no corresponding checkpoints or shadow branches
func (s *ManualCommitStrategy) ListOrphanedItems(ctx context.Context) ([]CleanupItem, error) {
	var items []CleanupItem

	// Shadow branches (should have been auto-cleaned after condensation)
	branches, err := ListShadowBranches(ctx)
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
