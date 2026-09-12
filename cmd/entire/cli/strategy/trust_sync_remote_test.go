package strategy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func newTrustIdentityRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
	testutil.AddRemote(t, dir, "fork", "https://github.com/me/widgets.git")
	return dir
}

func trustIdentityAt(ctx context.Context, t *testing.T, dir string) repopolicy.TrustIdentity {
	t.Helper()
	id, err := repopolicy.ResolveTrustIdentity(ctx, repopolicy.Repository{WorktreeRoot: dir})
	require.NoError(t, err)
	return id
}

// The consent identity follows the checkpoint sync remote election installed
// by this package — not origin — through every tier that can move it.
// Not parallel: uses t.Chdir().
func TestTrustIdentityFollowsElectedSyncRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("default tier is origin", func(t *testing.T) {
		dir := newTrustIdentityRepo(t)
		t.Chdir(dir)
		id := trustIdentityAt(ctx, t, dir)
		assert.Equal(t, "origin", id.RemoteName)
		assert.Equal(t, []string{"github.com/acme/widgets"}, id.OriginKeys)
	})

	t.Run("checkpoint_push_remote re-keys consent", func(t *testing.T) {
		dir := newTrustIdentityRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "fork")
		t.Chdir(dir)
		id := trustIdentityAt(ctx, t, dir)
		assert.Equal(t, "fork", id.RemoteName)
		assert.Equal(t, []string{"github.com/me/widgets"}, id.OriginKeys)
	})

	t.Run("captured election re-keys consent", func(t *testing.T) {
		dir := newTrustIdentityRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))
		id := trustIdentityAt(ctx, t, dir)
		assert.Equal(t, "fork", id.RemoteName)
		assert.Equal(t, []string{"github.com/me/widgets"}, id.OriginKeys)
	})

	t.Run("pending capture on this push wins over the current election", func(t *testing.T) {
		dir := newTrustIdentityRepo(t)
		t.Chdir(dir)
		id := trustIdentityAt(withPendingSyncRemote(ctx, "fork"), t, dir)
		assert.Equal(t, "fork", id.RemoteName)
		assert.Equal(t, []string{"github.com/me/widgets"}, id.OriginKeys)
	})

	t.Run("elected remote with a bare-path URL is path-keyed", func(t *testing.T) {
		dir := newTrustIdentityRepo(t)
		testutil.AddRemote(t, dir, "mirror", t.TempDir())
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "mirror")
		t.Chdir(dir)
		id := trustIdentityAt(ctx, t, dir)
		assert.Equal(t, "mirror", id.RemoteName)
		assert.False(t, id.OriginKeyed())
		assert.NotEmpty(t, id.Path)
	})

	t.Run("misconfigured checkpoint_push_remote is an identity error", func(t *testing.T) {
		dir := newTrustIdentityRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "gone")
		t.Chdir(dir)
		_, err := repopolicy.ResolveTrustIdentity(ctx, repopolicy.Repository{WorktreeRoot: dir})
		require.Error(t, err)
	})
}

// The held-checkpoint count shown after `entire trust --remote X` must be
// measured against X, not the election.
func TestResolveCheckpointSyncRemoteForTrust_HonorsOverride(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := newTrustIdentityRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	got, err := ResolveCheckpointSyncRemoteForTrust(ctx)
	require.NoError(t, err)
	assert.Equal(t, "origin", got.Name)
	got, err = ResolveCheckpointSyncRemoteForTrust(WithSyncRemoteOverride(ctx, "fork"))
	require.NoError(t, err)
	assert.Equal(t, "fork", got.Name)
}
