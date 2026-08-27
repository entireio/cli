package strategy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// TestHookCheckpointStoreOptions_CarriesBothBounds is the guard for the shape
// that broke: a hook store that fetches missing refs but not missing blobs
// resolves a checkpoint's ref and then reports the checkpoint itself missing on
// any partial clone.
func TestHookCheckpointStoreOptions_CarriesBothBounds(t *testing.T) {
	t.Parallel()

	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(context.Context, []plumbing.Hash) error { return nil })

	opts := s.hookCheckpointStoreOptions(context.Background())

	require.NotNil(t, opts.RefFetcher, "hook reads must be able to fetch a ref that exists only on the remote")
	require.NotNil(t, opts.BlobFetcher, "hook reads must be able to fetch a blob a partial clone filtered out")
}

func TestHookBlobFetcher_NilWithoutBlobFetcher(t *testing.T) {
	t.Parallel()

	s := NewManualCommitStrategy()

	require.Nil(t, s.hookBlobFetcher(), "no fetcher to bound means no fetcher to hand the store")
}

// TestHookBlobFetcher_BoundsTheCallAndPassesHashes pins the envelope the hook
// paths rely on: the interactive read chain's minutes must not be inherited by
// a hook the user's git command is waiting on, and SSH must not be able to
// prompt for a passphrase there.
func TestHookBlobFetcher_BoundsTheCallAndPassesHashes(t *testing.T) {
	t.Parallel()

	want := []plumbing.Hash{
		plumbing.NewHash("1111111111111111111111111111111111111111"),
		plumbing.NewHash("2222222222222222222222222222222222222222"),
	}
	sentinel := errors.New("fetch failed")

	var (
		gotHashes      []plumbing.Hash
		gotDeadline    time.Time
		gotHasDeadline bool
		gotBatchSSH    bool
		called         int
	)
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(ctx context.Context, hashes []plumbing.Hash) error {
		called++
		gotHashes = hashes
		gotDeadline, gotHasDeadline = ctx.Deadline()
		gotBatchSSH = remote.IsNonInteractiveSSH(ctx)
		return sentinel
	})

	fetch := s.hookBlobFetcher()
	require.NotNil(t, fetch)
	require.ErrorIs(t, fetch(context.Background(), want), sentinel, "the store must still see why a fetch failed")

	require.Equal(t, 1, called)
	require.Equal(t, want, gotHashes)
	require.True(t, gotBatchSSH, "a hook fetch must not be able to prompt for an SSH passphrase")
	require.True(t, gotHasDeadline, "a hook fetch must carry a deadline at all")
	require.WithinDuration(t, time.Now().Add(remote.WriteProbeFetchBudget), gotDeadline, 2*time.Second,
		"the hook fetch must carry the write-probe budget, not the interactive read chain's")
	require.Less(t, time.Until(gotDeadline), remote.ReadChainBudget,
		"the hook fetch must be bounded well inside the interactive read chain")
}

// TestHookBlobFetcher_DoesNotExtendACallerDeadline guards the direction that
// matters when the hook itself is already running out of time (an agent's
// session-end budget): bounding must never push a deadline out.
func TestHookBlobFetcher_DoesNotExtendACallerDeadline(t *testing.T) {
	t.Parallel()

	var (
		gotDeadline    time.Time
		gotHasDeadline bool
	)
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(ctx context.Context, _ []plumbing.Hash) error {
		gotDeadline, gotHasDeadline = ctx.Deadline()
		return nil
	})

	tight := 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), tight)
	defer cancel()
	require.NoError(t, s.hookBlobFetcher()(ctx, []plumbing.Hash{plumbing.ZeroHash}))

	// Assert the deadline EXISTS before comparing it. Without this the test is a
	// false positive in the direction that matters most: if the wrapper stopped
	// propagating a deadline, ctx.Deadline() returns (time.Time{}, false),
	// gotDeadline stays at the zero time — year 1 — and time.Until of that is
	// hugely negative, so the upper bound below is satisfied and an unbounded
	// hook fetch passes as bounded.
	require.True(t, gotHasDeadline, "bounding must not drop the caller's deadline entirely")
	require.LessOrEqual(t, time.Until(gotDeadline), tight)
}

// TestHookBlobFetcher_MemoizesBudgetExhaustion is the guard for the N-times
// stall the ref path already avoids and the blob path did not.
//
// gitRefsStore.fetchFailure memoizes a dead network for the store's lifetime,
// and its doc comment names the exact scenario: "a loop over N missing refs
// (e.g. a stop hook finalizing every checkpoint of a turn) pays a dead network
// once instead of N times". But it is consulted only in resolveRefMaybeFetch —
// the ref path. Blob reads go through NewFetchingTree, which holds no memo, so
// finalizeAllTurnCheckpoints looping TurnCheckpointIDs with one store paid
// WriteProbeFetchBudget per checkpoint (and File() fetches one hash at a time,
// so a read touching several missing blobs paid it several times). Nothing
// encloses that: newGitHookContext adds no deadline.
func TestHookBlobFetcher_MemoizesBudgetExhaustion(t *testing.T) {
	t.Parallel()

	var calls int
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(ctx context.Context, _ []plumbing.Hash) error {
		calls++
		<-ctx.Done() // a dead network: burn the whole budget
		return ctx.Err()
	})

	s.blobFetchBudget = 20 * time.Millisecond

	fetch := s.hookBlobFetcher()
	for range 5 {
		require.Error(t, fetch(context.Background(), []plumbing.Hash{plumbing.ZeroHash}))
	}

	require.Equal(t, 1, calls,
		"a budget-exhausted blob fetch must be memoized for the hook's lifetime; "+
			"otherwise a stop hook finalizing N checkpoints pays the budget N times")
}

// TestHookBlobFetcher_DoesNotMemoizeAFastFailure keeps the memo from poisoning
// recoverable reads. Only budget exhaustion — the dead-network signature, and
// the only failure expensive enough to be worth not repeating — is memoized. A
// fast error may be one blob's genuine absence, and must not stop the next
// blob from being fetched.
func TestHookBlobFetcher_DoesNotMemoizeAFastFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("remote has no such blob")
	var calls int
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(context.Context, []plumbing.Hash) error {
		calls++
		return sentinel
	})

	fetch := s.hookBlobFetcher()
	for range 3 {
		require.ErrorIs(t, fetch(context.Background(), []plumbing.Hash{plumbing.ZeroHash}), sentinel)
	}

	require.Equal(t, 3, calls, "a fast failure is cheap to repeat and may be per-blob absence")
}

// TestHookBlobFetcher_DoesNotMemoizeCallerCancellation mirrors the ref memo's
// guard: a cancellation originating from the CALLER's context says nothing
// about the remote and must not poison later fetches on this hook.
func TestHookBlobFetcher_DoesNotMemoizeCallerCancellation(t *testing.T) {
	t.Parallel()

	var calls int
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(ctx context.Context, _ []plumbing.Hash) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	})

	fetch := s.hookBlobFetcher()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, fetch(cancelled, []plumbing.Hash{plumbing.ZeroHash}))

	s.blobFetchBudget = 20 * time.Millisecond
	require.Error(t, fetch(context.Background(), []plumbing.Hash{plumbing.ZeroHash}))

	require.Equal(t, 2, calls, "caller cancellation must not memoize a network verdict")
}
