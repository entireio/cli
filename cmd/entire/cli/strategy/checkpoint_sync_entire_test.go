package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const testEntireURL = "entire://cluster.test/gh/acme/app"

// newEntireTestRepo builds a repo with an https origin and one entire://
// remote named "entire" — the topology the entire election tier is for.
func newEntireTestRepo(t *testing.T) string {
	t.Helper()
	dir := newCaptureTestRepo(t) // origin + fork, both https
	testutil.AddRemote(t, dir, "entire", testEntireURL)
	return dir
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_EntireTier(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("sole entire remote beats origin", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "entire", Source: SyncRemoteSourceEntire}, got)
	})

	t.Run("two entire remotes fall through to origin", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		testutil.AddRemote(t, dir, "entire-eu", "entire://eu.cluster.test/gh/acme/app")
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got,
			"the tier must not guess between several Entire remotes")
	})

	t.Run("two entire remotes and no origin fall through to first", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		testutil.WriteFile(t, dir, "f.txt", "init")
		testutil.GitAdd(t, dir, "f.txt")
		testutil.GitCommit(t, dir, "init")
		testutil.AddRemote(t, dir, "entire-a", "entire://a.cluster.test/gh/acme/app")
		testutil.AddRemote(t, dir, "entire-b", "entire://b.cluster.test/gh/acme/app")
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "entire-a", Source: SyncRemoteSourceFirst}, got)
	})

	t.Run("explicit checkpoint_push_remote beats entire", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "fork")
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceConfig}, got)
	})

	t.Run("captured remote beats entire", func(t *testing.T) {
		// Product order: a capture already in force is a decision already
		// made. Setting checkpoint_push_remote to the Entire remote re-routes by writing the
		// explicit setting, which outranks both.
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceObserved}, got)
	})

	t.Run("raw entire URL is read even when insteadOf rewrites transport", func(t *testing.T) {
		// Production: raw config says entire://, the helper does transport.
		// Tests: raw config says entire://, insteadOf sends git to a bare repo.
		// Both need detection to read the RAW value — `git remote get-url`
		// expands insteadOf and would report the rewritten URL.
		dir := newEntireTestRepo(t)
		setGitConfig(t, dir, "url.file:///nonexistent/bare.insteadOf", testEntireURL)
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "entire", Source: SyncRemoteSourceEntire}, got)
		assert.True(t, isConfiguredRemote(ctx, "entire"), "membership still answers through git")
	})

	t.Run("a remote with a non-entire pushurl is not an Entire remote", func(t *testing.T) {
		// Fetch from Entire, push to GitHub: pushes fan out to GitHub, so the
		// tier must not elect it — transcripts would land on GitHub.
		dir := newEntireTestRepo(t)
		gitInRepo(t, dir, "remote", "set-url", "--push", "entire", "https://github.com/acme/app.git")
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
		assert.Empty(t, EntireRemotes(ctx))
	})

	t.Run("a remote with a second non-entire url is not an Entire remote", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		gitInRepo(t, dir, "remote", "set-url", "--add", "entire", "https://github.com/acme/app.git")
		t.Chdir(dir)

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
	})
}

func TestReadRemotesInConfigOrder_RetainsURLs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "entire", testEntireURL)
	gitInRepo(t, dir, "remote", "set-url", "--add", "--push", "origin", "https://example.com/mirror.git")
	// pushurl-only remotes stay invisible (spec Unit 1).
	gitInRepo(t, dir, "config", "remote.pushonly.pushurl", "https://example.com/pushonly.git")
	ctx := settings.WithWorktreeRoot(context.Background(), dir)

	got, err := readRemotesInConfigOrder(ctx)
	require.NoError(t, err)
	assert.Equal(t, []configuredRemote{
		{Name: "origin", URLs: []string{"https://example.com/origin.git"}, PushURLs: []string{"https://example.com/mirror.git"}},
		{Name: "entire", URLs: []string{testEntireURL}},
	}, got)
	assert.Equal(t, []string{"origin", "entire"}, configuredRemotesInConfigOrder(ctx))
	assert.Equal(t, []string{"entire"}, EntireRemotes(ctx))
	assert.Equal(t, "origin", LegacyCheckpointRemote(ctx))
}

func TestReadRemotesInConfigOrder_NoRemotesIsEmptyNotError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	ctx := settings.WithWorktreeRoot(context.Background(), dir)

	got, err := readRemotesInConfigOrder(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestEntireRemotesOf(t *testing.T) {
	t.Parallel()
	remotes := []configuredRemote{
		{Name: "origin", URLs: []string{"https://github.com/a/b.git"}},
		{Name: "entire", URLs: []string{testEntireURL}},
		{Name: "mixed", URLs: []string{testEntireURL}, PushURLs: []string{"https://github.com/a/b.git"}},
		{Name: "two", URLs: []string{testEntireURL, "https://github.com/a/b.git"}},
		{Name: "entire-push", URLs: []string{testEntireURL}, PushURLs: []string{testEntireURL}},
		{Name: "pushonly", PushURLs: []string{testEntireURL}},
	}
	assert.Equal(t, []string{"entire", "entire-push"}, entireRemotesOf(remotes))
}

func TestLegacyCheckpointRemote_PicksNonEntire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  []configuredRemote
		want string
	}{
		{"origin wins", remotesNamed("fork", "origin"), "origin"},
		{"sole non-entire", []configuredRemote{{Name: "entire", URLs: []string{testEntireURL}}, {Name: "fork", URLs: []string{"https://x/y.git"}}}, "fork"},
		{"first non-entire", []configuredRemote{{Name: "entire", URLs: []string{testEntireURL}}, {Name: "b", URLs: []string{"https://x/b.git"}}, {Name: "a", URLs: []string{"https://x/a.git"}}}, "b"},
		{"all entire", []configuredRemote{{Name: "entire", URLs: []string{testEntireURL}}}, ""},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := repoScopedCache(t)
			// Seed the cache with the fixture so no git runs.
			cachedRemotesInConfigOrder(ctx, func(context.Context) ([]configuredRemote, error) { return tc.got, nil })
			assert.Equal(t, tc.want, LegacyCheckpointRemote(ctx))
		})
	}
}

func TestConfiguredEntireRemotes_SharesTheCachedRead(t *testing.T) {
	t.Parallel()
	ctx := repoScopedCache(t)
	calls := 0
	read := okRead(&calls, []configuredRemote{{Name: "entire", URLs: []string{testEntireURL}}})
	cachedRemotesInConfigOrder(ctx, read)
	require.Equal(t, 1, calls)

	assert.Equal(t, []string{"entire"}, configuredEntireRemotes(ctx))
	assert.Equal(t, []string{"entire"}, configuredRemotesInConfigOrder(ctx))
	assert.Equal(t, 1, calls, "the entire tier must be served from the one memoized .git/config read")
}

// Not parallel: uses t.Chdir()
func TestCaptureCheckpointSyncRemote_EntireElectionNotDisplaceable(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	dir := newEntireTestRepo(t)
	// The branch declares fork: under the default tiers this push would
	// capture. Under the entire tier it must not move checkpoints off Entire.
	setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
	t.Chdir(dir)
	buf := captureStderrWriter(t)

	assert.False(t, pendingCaptureCheckpointSyncRemote(ctx, "fork"))
	captureOnSuccessfulPush(ctx, "fork")

	assert.Empty(t, loadCapturedSyncRemotes(ctx), "an Entire election is not displaceable by a push habit")
	assert.Empty(t, buf.String())
	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "entire", Source: SyncRemoteSourceEntire}, got)
}

// Not parallel: uses t.Chdir()
func TestRedirectToEntireSyncRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("no entire remote: untouched and not in force", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		ps := pushSettings{remote: "origin"}

		_, entireTier := redirectToEntireSyncRemote(ctx, &ps)

		assert.False(t, entireTier)
		assert.Equal(t, pushSettings{remote: "origin"}, ps)
		assert.Equal(t, "origin", ps.pushTarget())
	})

	t.Run("push to the entire remote itself: in force, no redirect", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		ps := pushSettings{remote: "entire"}

		elected, entireTier := redirectToEntireSyncRemote(ctx, &ps)

		assert.True(t, entireTier)
		assert.Equal(t, "entire", elected.Name)
		assert.False(t, ps.redirectedToSyncRemote())
		assert.Equal(t, "entire", ps.pushTarget())
	})

	t.Run("push to origin is redirected to the entire remote", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		ps := pushSettings{remote: "origin"}

		_, entireTier := redirectToEntireSyncRemote(ctx, &ps)

		assert.True(t, entireTier)
		assert.True(t, ps.redirectedToSyncRemote())
		assert.Equal(t, "entire", ps.pushTarget())
		assert.Equal(t, "origin", ps.remote, "the remote the user pushed is kept for reference")
	})

	t.Run("raw URL push is redirected too", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		ps := pushSettings{remote: "https://example.com/elsewhere.git"}

		_, entireTier := redirectToEntireSyncRemote(ctx, &ps)

		assert.True(t, entireTier)
		assert.Equal(t, "entire", ps.pushTarget())
	})

	t.Run("dedicated checkpoint URL wins and the tier stays out", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		ps := pushSettings{remote: "origin", checkpointURL: "https://example.com/cp.git"}

		_, entireTier := redirectToEntireSyncRemote(ctx, &ps)

		assert.False(t, entireTier)
		assert.Equal(t, "https://example.com/cp.git", ps.pushTarget())
	})

	t.Run("explicit checkpoint_push_remote keeps the ordinary gate", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "fork")
		t.Chdir(dir)
		ps := pushSettings{remote: "origin"}

		_, entireTier := redirectToEntireSyncRemote(ctx, &ps)

		assert.False(t, entireTier)
		assert.Equal(t, "origin", ps.pushTarget())
	})
}

// Not parallel: uses t.Chdir()
func TestDeferCheckpointPushOnEmptyRemote_SkippedWhenRedirected(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := newEntireTestRepo(t)
	t.Chdir(dir)
	ctx := context.Background()

	// No tracking refs anywhere: an ordinary origin push would defer.
	require.True(t, deferCheckpointPushOnEmptyRemote(ctx, pushSettings{remote: "origin"}))
	// Redirected to the Entire remote: never defer — the user's code pushes
	// would never create refs/remotes/entire/*, so the defer would be forever.
	assert.False(t, deferCheckpointPushOnEmptyRemote(ctx, pushSettings{remote: "origin", syncRemote: "entire"}))
}

// Not parallel: uses t.Chdir()
func TestAnnounceEntireSyncRemoteOnce(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("first delivery announces and names the legacy remote; second is silent", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		announceEntireSyncRemoteOnce(ctx, "entire")

		out := buf.String()
		assert.Contains(t, out, `[entire] Checkpoints now sync to "entire" — your Entire remote.`)
		assert.NotContains(t, out, "Earlier checkpoints", "no backlog pointer until the migration command exists")
		st, ok := LoadEntireSyncState(ctx)
		require.True(t, ok)
		assert.Equal(t, "entire", st.Remote)
		assert.False(t, st.AnnouncedAt.IsZero())
		assert.Equal(t, EntireSyncMigrationNone, st.Migration)

		buf.Reset()
		announceEntireSyncRemoteOnce(ctx, "entire")
		assert.Empty(t, buf.String(), "once per clone")
	})

	t.Run("a recorded migration drops the legacy line", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, MarkEntireSyncMigration(ctx, EntireSyncMigrationDone, "origin"))
		buf := captureStderrWriter(t)

		announceEntireSyncRemoteOnce(ctx, "entire")

		out := buf.String()
		assert.Contains(t, out, `Checkpoints now sync to "entire"`)
		assert.NotContains(t, out, "Earlier checkpoints")
		st, ok := LoadEntireSyncState(ctx)
		require.True(t, ok)
		assert.Equal(t, EntireSyncMigrationDone, st.Migration, "the announcement must not clobber the migration verdict")
		assert.Equal(t, "origin", st.MigratedFrom)
		assert.False(t, st.MigratedAt.IsZero())
	})

	t.Run("no legacy remote: only the first line", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		testutil.WriteFile(t, dir, "f.txt", "init")
		testutil.GitAdd(t, dir, "f.txt")
		testutil.GitCommit(t, dir, "init")
		testutil.AddRemote(t, dir, "entire", testEntireURL)
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		announceEntireSyncRemoteOnce(ctx, "entire")

		assert.Contains(t, buf.String(), `Checkpoints now sync to "entire"`)
		assert.NotContains(t, buf.String(), "Earlier checkpoints")
	})

	t.Run("a different entire remote re-announces", func(t *testing.T) {
		dir := newEntireTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, SaveEntireSyncState(ctx, EntireSyncState{Remote: "old-entire", AnnouncedAt: time.Now().Add(-time.Hour)}))
		buf := captureStderrWriter(t)

		announceEntireSyncRemoteOnce(ctx, "entire")

		assert.Contains(t, buf.String(), `Checkpoints now sync to "entire"`)
	})
}

// Not parallel: uses t.Chdir()
func TestLoadEntireSyncState_MissingOrCorruptIsZero(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	dir := newEntireTestRepo(t)
	t.Chdir(dir)

	st, ok := LoadEntireSyncState(ctx)
	assert.False(t, ok)
	assert.Equal(t, EntireSyncState{}, st)

	root, err := capturedSyncRemotesRoot(ctx)
	require.NoError(t, err)
	require.NoError(t, SaveEntireSyncState(ctx, EntireSyncState{Remote: "entire"}))
	st, ok = LoadEntireSyncState(ctx)
	assert.True(t, ok)
	assert.Equal(t, "entire", st.Remote)

	require.NoError(t, root.Remove(entireSyncStateFileName))
	require.NoError(t, root.WriteFile(entireSyncStateFileName, []byte("{not json"), 0o600))
	st, ok = LoadEntireSyncState(ctx)
	assert.False(t, ok)
	assert.Equal(t, EntireSyncState{}, st)
}

// Not parallel: uses t.Chdir()
func TestHintGatedCheckpointSync_SeveralEntireRemotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	initRepo := func(t *testing.T) string {
		dir, _, second := initCountTestRepo(t)
		testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, dir, "entire-a", "entire://a.cluster.test/gh/acme/app")
		testutil.AddRemote(t, dir, "entire-b", "entire://b.cluster.test/gh/acme/app")
		testutil.GitUpdateRef(t, dir, v1LocalRef, second)
		return dir
	}

	t.Run("push to one of several entire remotes names the setting for that remote", func(t *testing.T) {
		dir := initRepo(t)
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "entire-a")

		out := buf.String()
		assert.Contains(t, out, `"origin"`, "names the elected destination")
		assert.Contains(t, out, "2 Entire remotes")
		assert.Contains(t, out, `set strategy_options.checkpoint_push_remote to "entire-a" in .entire/settings.local.json`)
	})

	t.Run("nothing waiting stays silent", func(t *testing.T) {
		dir := initRepo(t)
		testutil.GitUpdateRef(t, dir, "refs/remotes/origin/"+paths.MetadataBranchName, testutil.GetHeadHash(t, dir))
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "entire-a")

		assert.Empty(t, buf.String())
	})

	t.Run("push to the non-entire origin keeps the ordinary hint", func(t *testing.T) {
		dir := initRepo(t)
		// origin is elected (two entire remotes, tier declines), so a push to
		// origin is not gated at all; use a third plain remote to reach the
		// ordinary hint and confirm it is unchanged.
		testutil.AddRemote(t, dir, "publish", "https://example.com/publish.git")
		setGitConfig(t, dir, "remote.pushDefault", "publish")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		assert.Contains(t, buf.String(), "checkpoint_push_remote")
	})
}
