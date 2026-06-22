package strategy

import (
	"context"
	"fmt"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
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

	// blobFetcher, when set, is passed to the checkpoint store to enable
	// on-demand blob fetching after treeless fetches. Set via SetBlobFetcher.
	blobFetcher checkpoint.BlobFetchFunc
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

func (s *ManualCommitStrategy) getCheckpointStores(ctx context.Context, repo *git.Repository) (*checkpoint.Stores, error) {
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: s.blobFetcher})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	return stores, nil
}

// getCheckpointStore returns a store bound to the resolved committed-metadata
// topology. Writes target refs.Primary; reads target refs.Read. The strategy's
// blob fetcher is wired in so reads can fetch blobs on demand after a treeless
// fetch.
func (s *ManualCommitStrategy) getCheckpointStore(ctx context.Context, repo *git.Repository) (checkpoint.CommittedStore, error) { //nolint:ireturn // committed store capability is the abstraction boundary
	stores, err := s.getCheckpointStores(ctx, repo)
	if err != nil {
		return nil, err
	}
	return stores.Primary, nil
}

// getTemporaryStore returns the git-backed shadow-branch store with the
// strategy's blob fetcher wired in.
func (s *ManualCommitStrategy) getTemporaryStore(ctx context.Context, repo *git.Repository) (checkpoint.TemporaryStore, error) { //nolint:ireturn // temporary store capability is the abstraction boundary
	stores, err := s.getCheckpointStores(ctx, repo)
	if err != nil {
		return nil, err
	}
	return stores.Temporary(), nil
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
	defer repo.Close()

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
