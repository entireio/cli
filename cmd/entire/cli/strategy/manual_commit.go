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

// withBlobFetcher wires the strategy's blob fetcher into a store so it can fetch
// blobs on demand after a treeless fetch.
func (s *ManualCommitStrategy) withBlobFetcher(store *checkpoint.GitStore) *checkpoint.GitStore {
	if s.blobFetcher != nil {
		store.SetBlobFetcher(s.blobFetcher)
	}
	return store
}

// getCheckpointStore returns the v1-branch store used by write paths.
func (s *ManualCommitStrategy) getCheckpointStore(repo *git.Repository) *checkpoint.GitStore {
	return s.withBlobFetcher(checkpoint.NewGitStore(repo))
}

// getCommittedReadStore returns a store for reading committed checkpoints, bound
// to the read ref for the active checkpoints_version (the local-only v1.1 custom
// ref when opted in, else the v1 branch). Use this for committed reads;
// getCheckpointStore is for writes.
func (s *ManualCommitStrategy) getCommittedReadStore(ctx context.Context, repo *git.Repository) *checkpoint.GitStore {
	return s.withBlobFetcher(checkpoint.NewCommittedReadStore(ctx, repo))
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
