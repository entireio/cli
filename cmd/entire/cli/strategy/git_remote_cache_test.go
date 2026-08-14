package strategy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestGitRemoteCache_OnlyMemoizesWhenInstalled pins the opt-in property that
// makes this safe: an uninstrumented context reads git every time, exactly as
// before, so tests (which pass plain contexts) cannot leak one temp repo's
// remote list into another.
func TestGitRemoteCache_OnlyMemoizesWhenInstalled(t *testing.T) {
	t.Parallel()

	t.Run("no cache installed: every call reads", func(t *testing.T) {
		t.Parallel()
		calls := 0
		probe := func() bool { calls++; return true }
		ctx := context.Background()

		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.Equal(t, 2, calls, "without a cache the probe must run every time")
	})

	t.Run("cache installed: one read per remote name", func(t *testing.T) {
		t.Parallel()
		calls := 0
		probe := func() bool { calls++; return true }
		ctx := WithGitRemoteCache(context.Background())

		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.True(t, cachedIsConfiguredRemote(ctx, "origin", probe))
		assert.Equal(t, 1, calls, "the second lookup of the same name must be free")

		cachedIsConfiguredRemote(ctx, "fork", probe)
		assert.Equal(t, 2, calls, "a different name is a different question")
	})

	t.Run("negative answers are cached too", func(t *testing.T) {
		t.Parallel()
		calls := 0
		probe := func() bool { calls++; return false }
		ctx := WithGitRemoteCache(context.Background())

		assert.False(t, cachedIsConfiguredRemote(ctx, "gone", probe))
		assert.False(t, cachedIsConfiguredRemote(ctx, "gone", probe))
		assert.Equal(t, 1, calls, "a missing remote must not be re-probed")
	})
}

// TestGitRemoteCache_EmptyListIsCached: an empty remote list is a real answer,
// not a cache miss — otherwise a repo with no remotes re-shells out forever.
func TestGitRemoteCache_EmptyListIsCached(t *testing.T) {
	t.Parallel()

	calls := 0
	read := func() []string { calls++; return nil }
	ctx := WithGitRemoteCache(context.Background())

	assert.Empty(t, cachedRemotesInConfigOrder(ctx, read))
	assert.Empty(t, cachedRemotesInConfigOrder(ctx, read))
	assert.Equal(t, 1, calls, "a legitimately empty list must still be cached")
}

func TestGitRemoteCache_Invalidate(t *testing.T) {
	t.Parallel()

	listCalls, probeCalls := 0, 0
	read := func() []string { listCalls++; return []string{"origin"} }
	probe := func() bool { probeCalls++; return true }
	ctx := WithGitRemoteCache(context.Background())

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
