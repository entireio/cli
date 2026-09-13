package strategy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
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

	// blobFetchBudget bounds one blob fetch on a git-hook path. Zero means
	// remote.WriteProbeFetchBudget. Per-instance rather than a package var so
	// tests can shorten it without mutating process-global state, which
	// t.Parallel() makes a data race (CLAUDE.md: "Tests that modify
	// process-global state cannot be parallelized").
	blobFetchBudget time.Duration

	// turnCheckpointFinalizeBudget bounds the whole turn-finalize pass. Zero
	// means the production default.
	turnCheckpointFinalizeBudget time.Duration

	// checkpointRefFetcher overrides the hook ref fetcher in tests. Production
	// uses remote.HookCheckpointRefFetcher.
	checkpointRefFetcher checkpoint.RefFetchFunc
}

const (
	standardTurnCheckpointFinalizeBudget    = 20 * time.Second
	constrainedTurnCheckpointFinalizeBudget = 5 * time.Second
)

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
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: s.blobFetcher, ReadRemotes: CheckpointReadRemotes(ctx)})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	return stores, nil
}

// getPersistentStore returns a store bound to the resolved committed-metadata
// topology. Writes target refs.Primary; reads target refs.Read. The strategy's
// blob fetcher is wired in so reads can fetch blobs on demand after a treeless
// fetch.
func (s *ManualCommitStrategy) getPersistentStore(ctx context.Context, repo *git.Repository) (checkpoint.PersistentStore, error) {
	stores, err := s.getCheckpointStores(ctx, repo)
	if err != nil {
		return nil, err
	}
	return stores.Persistent, nil
}

// getEphemeralStore returns the git-backed shadow-branch store with the
// strategy's blob fetcher wired in.
func (s *ManualCommitStrategy) getEphemeralStore(ctx context.Context, repo *git.Repository) (checkpoint.EphemeralStore, error) {
	stores, err := s.getCheckpointStores(ctx, repo)
	if err != nil {
		return nil, err
	}
	return stores.Ephemeral(), nil
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

// hookCheckpointStoreOptions is the store envelope for a git-hook read: the
// bounded ref probe, the bounded blob fetch, and the read-candidate chain. The
// two bounds live together because a hook that has one and not the other still
// fails — a ref fetcher with no blob fetcher resolves the checkpoint's ref and
// then reads its filtered-out metadata.json as absent, reporting a checkpoint
// that exists as missing.
func (s *ManualCommitStrategy) hookCheckpointStoreOptions(ctx context.Context) checkpoint.OpenOptions {
	refFetcher := s.checkpointRefFetcher
	if refFetcher == nil {
		refFetcher = remote.HookCheckpointRefFetcher()
	}
	return checkpoint.OpenOptions{
		RefFetcher:  refFetcher,
		BlobFetcher: s.hookBlobFetcher(),
		ReadRemotes: CheckpointReadRemotes(ctx),
	}
}

// hookBlobFetcher returns the strategy's blob fetcher bounded for git-hook
// contexts: the write-probe budget over the whole call plus BatchMode SSH, the
// same envelope remote.HookCheckpointRefFetcher puts around the ref probe those
// hooks already run. Reads there must not inherit the interactive read chain's
// minutes while a user's git command waits on the hook.
//
// It fires only when a checkpoint blob is genuinely absent from the local
// object store, which on a full clone never happens — the cost lands on
// partial clones, where the alternative is not "no network" but a checkpoint
// read that reports the checkpoint missing.
//
// The returned fetcher also memoizes budget exhaustion for its own lifetime
// (one hook invocation), which is the blob-side counterpart of
// gitRefsStore.fetchFailure. The ref memo is consulted only in
// resolveRefMaybeFetch, so it does not cover blob reads: those go through
// NewFetchingTree, which holds no memo. Without one, finalizeAllTurnCheckpoints
// looping state.TurnCheckpointIDs against a single store paid
// WriteProbeFetchBudget PER CHECKPOINT on a dead network — and FetchingTree.File
// fetches one hash at a time, so a read touching several missing blobs paid it
// several times. Nothing bounds the total: newGitHookContext adds no deadline,
// and an agent kills the hook long before the loop ends.
//
// Only budget exhaustion is memoized, deliberately. It is the dead-network
// signature and the only failure expensive enough to be worth not repeating; a
// fast error is cheap to retry and may be one blob's genuine remote absence,
// which must not stop the next blob from being fetched. As in the ref memo, a
// cancellation originating from the CALLER says nothing about the remote and is
// never memoized.
func (s *ManualCommitStrategy) hookBlobFetcher() checkpoint.BlobFetchFunc {
	if s.blobFetcher == nil {
		return nil
	}
	var (
		mu        sync.Mutex
		exhausted error
	)
	return func(ctx context.Context, hashes []plumbing.Hash) error {
		// The lock covers the read and is then released — deliberately not held
		// across the check and fetch below. Two things forbid widening it: the
		// fetch is a bounded network call, and the memo write further down
		// re-acquires this same non-reentrant mutex, so holding it here
		// deadlocks. That leaves a check-then-act window where two concurrent
		// first-callers would both fetch, which is acceptable: every caller on
		// these paths is sequential (finalizeAllTurnCheckpoints loops one
		// checkpoint at a time; FetchingTree.File and PreFetch are sequential
		// within a read), and if it were ever reached the cost is one duplicate
		// fetch, not a wrong answer.
		mu.Lock()
		prior := exhausted
		mu.Unlock()
		if prior != nil {
			return prior
		}

		budget := s.blobFetchBudget
		if budget == 0 {
			budget = remote.WriteProbeFetchBudget
		}
		fetchCtx, cancel := context.WithTimeout(remote.WithNonInteractiveSSH(ctx), budget)
		defer cancel()
		err := s.blobFetcher(fetchCtx, hashes)
		if err != nil && ctx.Err() == nil && errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
			mu.Lock()
			if exhausted == nil {
				exhausted = err
			}
			mu.Unlock()
		}
		return err
	}
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
