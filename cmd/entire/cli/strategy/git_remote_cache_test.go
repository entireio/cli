package strategy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// repoScopedCache installs a cache and pins the repository explicitly. Without the
// pin, gitReadRepo falls back to resolving the repo from the test process's cwd, so
// whether caching happens at all would depend on `go test` running inside a git
// worktree — and these assertions would fail for reasons unrelated to the cache.
func repoScopedCache(t *testing.T) context.Context {
	t.Helper()
	return settings.WithWorktreeRoot(WithGitRemoteCache(context.Background()), t.TempDir())
}

// okRead / okProbe are counting callbacks that succeed, for the memoization
// assertions. Failure is covered separately by
// TestGitRemoteCache_FailedReadsAreNotMemoized.
func okRead(calls *int, names []string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { *calls++; return names, nil }
}

func okProbe(calls *int, got bool) func() (bool, error) {
	return func() (bool, error) { *calls++; return got, nil }
}

// TestGitRemoteCache_FailedReadsAreNotMemoized is the regression guard for the
// sticky-wrong-answer hazard: both underlying git reads used to erase failure to
// nil/false, so caching one transient fork failure or cancelled context made the
// election answer "this repo has no remotes" — and silently skip checkpoint sync —
// for the rest of the process.
func TestGitRemoteCache_FailedReadsAreNotMemoized(t *testing.T) {
	t.Parallel()

	ctx := repoScopedCache(t)
	boom := errors.New("git could not run")

	listCalls := 0
	flakyList := func(context.Context) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return nil, boom
		}
		return []string{"origin"}, nil
	}
	assert.Empty(t, cachedRemotesInConfigOrder(ctx, flakyList), "a failed read answers empty for this call")
	assert.Equal(t, []string{"origin"}, cachedRemotesInConfigOrder(ctx, flakyList),
		"the failure must not be memoized; the next call retries and sees the real list")

	probeCalls := 0
	flakyProbe := func() (bool, error) {
		probeCalls++
		if probeCalls == 1 {
			return false, boom
		}
		return true, nil
	}
	assert.False(t, cachedIsConfiguredRemote(ctx, "origin", flakyProbe))
	assert.True(t, cachedIsConfiguredRemote(ctx, "origin", flakyProbe),
		"a failed probe must not fail-close checkpoint_push_remote for the rest of the process")
}

// TestGitRemoteCache_OnlyMemoizesWhenInstalled pins the opt-in property that
// makes this safe: an uninstrumented context reads git every time, exactly as
// before, so tests (which pass plain contexts) cannot leak one temp repo's
// remote list into another.
func TestGitRemoteCache_OnlyMemoizesWhenInstalled(t *testing.T) {
	t.Parallel()

	t.Run("no cache installed: every call reads", func(t *testing.T) {
		t.Parallel()
		calls := 0
		probe := okProbe(&calls, true)
		ctx := settings.WithWorktreeRoot(context.Background(), t.TempDir())

		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.Equal(t, 2, calls, "without a cache the probe must run every time")
	})

	t.Run("cache installed: one read per remote name", func(t *testing.T) {
		t.Parallel()
		calls := 0
		probe := okProbe(&calls, true)
		ctx := repoScopedCache(t)

		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.Equal(t, 1, calls, "the second lookup of the same name must be free")

		cachedIsConfiguredRemote(ctx, "fork", probe)
		assert.Equal(t, 2, calls, "a different name is a different question")
	})

	t.Run("negative answers are cached too", func(t *testing.T) {
		t.Parallel()
		calls := 0
		probe := okProbe(&calls, false)
		ctx := repoScopedCache(t)

		assert.False(t, cachedIsConfiguredRemote(ctx, "gone", probe))
		assert.False(t, cachedIsConfiguredRemote(ctx, "gone", probe))
		assert.Equal(t, 1, calls, "a missing remote must not be re-probed")
	})
}

// TestGitRemoteCache_PartitionsByRepository is the regression guard for the
// cross-repo hazard: `entire dispatch --repos a,b` walks several repositories in
// ONE process, scoping each election with settings.WithWorktreeRoot. A cache keyed
// only by remote name answers repo B from repo A's remote list, silently
// re-breaking c04a2e312 ("honor read candidates per repository").
func TestGitRemoteCache_PartitionsByRepository(t *testing.T) {
	t.Parallel()

	ctx := WithGitRemoteCache(context.Background())
	repoA := settings.WithWorktreeRoot(ctx, t.TempDir())
	repoB := settings.WithWorktreeRoot(ctx, t.TempDir())

	// Same remote name, opposite answers, one shared cache.
	assert.True(t, cachedIsConfiguredRemote(repoA, "origin", func() (bool, error) { return true, nil }))
	assert.False(t, cachedIsConfiguredRemote(repoB, "origin", func() (bool, error) { return false, nil }),
		"repo B must not inherit repo A's membership answer")

	listA := func(context.Context) ([]string, error) { return []string{"origin"}, nil }
	listB := func(context.Context) ([]string, error) { return []string{"fork", "upstream"}, nil }
	assert.Equal(t, []string{"origin"}, cachedRemotesInConfigOrder(repoA, listA))
	assert.Equal(t, []string{"fork", "upstream"}, cachedRemotesInConfigOrder(repoB, listB),
		"repo B must not inherit repo A's remote list")

	// Each repo still memoizes within itself.
	calls := 0
	countingA := okRead(&calls, nil)
	cachedRemotesInConfigOrder(repoA, countingA)
	assert.Equal(t, 0, calls, "repo A's list was already cached; the partition must not defeat memoization")
}

// TestGitRemoteCache_CrossRepoReadsDoNotSerialize pins that a git read for one
// repository never blocks another's. `entire dispatch --repos a,b` fans out over
// repositories with an errgroup (dispatch/mode_local.go), so a single lock held
// across the subprocess would serialize work that ran concurrently before this
// cache existed.
//
// Deterministic rather than timing-based, and ordered so the outcome cannot depend
// on which goroutine wins the start: A enters its read FIRST and only then is B
// allowed to call. With per-repository locks B proceeds and releases A; with one
// global lock held across the read B blocks, A waits forever on B, and the test
// fails on the deadline instead of hanging.
func TestGitRemoteCache_CrossRepoReadsDoNotSerialize(t *testing.T) {
	t.Parallel()

	ctx := WithGitRemoteCache(context.Background())
	repoA := settings.WithWorktreeRoot(ctx, t.TempDir())
	repoB := settings.WithWorktreeRoot(ctx, t.TempDir())

	aInside := make(chan struct{})
	bStarted := make(chan struct{})
	done := make(chan struct{}, 2)

	go func() {
		cachedRemotesInConfigOrder(repoA, func(context.Context) ([]string, error) {
			close(aInside)
			<-bStarted // A cannot finish until B is inside its own read
			return []string{"origin"}, nil
		})
		done <- struct{}{}
	}()
	go func() {
		<-aInside // B starts only once A is demonstrably inside its read
		cachedRemotesInConfigOrder(repoB, func(context.Context) ([]string, error) {
			close(bStarted)
			return []string{"fork"}, nil
		})
		done <- struct{}{}
	}()

	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("cross-repository reads serialized: one repository's git read blocked another's")
		}
	}
}

// TestGitRemoteCache_EmptyListIsCached: an empty remote list is a real answer,
// not a cache miss — otherwise a repo with no remotes re-shells out forever.
func TestGitRemoteCache_EmptyListIsCached(t *testing.T) {
	t.Parallel()

	calls := 0
	read := okRead(&calls, nil)
	ctx := repoScopedCache(t)

	assert.Empty(t, cachedRemotesInConfigOrder(ctx, read))
	assert.Empty(t, cachedRemotesInConfigOrder(ctx, read))
	assert.Equal(t, 1, calls, "a legitimately empty list must still be cached")
}

func TestGitRemoteCache_Invalidate(t *testing.T) {
	t.Parallel()

	listCalls, probeCalls := 0, 0
	read := okRead(&listCalls, []string{"origin"})
	probe := okProbe(&probeCalls, true)
	ctx := repoScopedCache(t)

	cachedRemotesInConfigOrder(ctx, read)
	cachedIsConfiguredRemote(ctx, "origin", probe)
	require.Equal(t, 1, listCalls)
	require.Equal(t, 1, probeCalls)

	InvalidateGitRemoteCache(ctx)

	cachedRemotesInConfigOrder(ctx, read)
	cachedIsConfiguredRemote(ctx, "origin", probe)
	assert.Equal(t, 2, listCalls, "invalidation must force a re-read")
	assert.Equal(t, 2, probeCalls, "invalidation must clear membership answers too")

	// Invalidating a context without a cache must not panic.
	InvalidateGitRemoteCache(context.Background())
}

// TestGitRemoteCache_ElectionSeesRemoteAddedAfterInvalidate is the end-to-end
// guard for the hazard the cache introduces: a remote added mid-invocation must
// be visible to a later election once the mutator invalidates. `entire repo
// mirror use` is the only production mutator, and it calls
// InvalidateGitRemoteCache for exactly this reason.
//
// Not parallel: t.Chdir and IsolateGitConfigEnv touch process-global state.
func TestGitRemoteCache_ElectionSeesRemoteAddedAfterInvalidate(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	ctx := WithGitRemoteCache(t.Context())
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")

	elected, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	require.Equal(t, "origin", elected.Name)

	// A second remote appears after the first election cached the list. Without
	// invalidation the election would keep answering from the stale snapshot.
	testutil.AddRemote(t, dir, "fork", "https://example.com/fork.git")
	InvalidateGitRemoteCache(ctx)

	assert.True(t, isConfiguredRemote(ctx, "fork"),
		"a remote added after invalidation must be visible to the election")
}
