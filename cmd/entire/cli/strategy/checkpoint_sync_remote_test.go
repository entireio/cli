package strategy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// currentBranchName returns the short name of the current branch in repoDir.
func currentBranchName(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// setGitConfig sets a git config key to value in repoDir.
func setGitConfig(t *testing.T, repoDir, key, value string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "config", key, value)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSetting(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingMissingRemote_FailsClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gone")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_DefaultsToOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SoleRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "upstream", Source: SyncRemoteSourceSole}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_FirstInConfigOrder(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// No "origin" remote. Add zeta before alpha; config-file order should win
	// over alphabetical order.
	testutil.AddRemote(t, tmpDir, "zeta", "https://example.com/zeta.git")
	testutil.AddRemote(t, tmpDir, "alpha", "https://example.com/alpha.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "zeta", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SettingsLoadErrorFailsClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	// Corrupt settings.json: the file may contain a checkpoint_push_remote
	// we cannot read, so election must not proceed.
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot read settings")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_NoRemotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_PushurlOnlyRemoteIsInvisible(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// A remote configured with only a pushurl (no url) added first. If it
	// were counted, it would sort first in .git/config order and get elected.
	cmd := exec.CommandContext(ctx, "git", "config", "remote.pushonly.pushurl", "https://example.com/pushonly.git")
	cmd.Dir = tmpDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Two real remotes added after it, no "origin" — this keeps the visible
	// remote count at 2 so the resolver exercises the "first" precedence
	// path (not "sole"), proving the pushurl-only entry is excluded from
	// both the count and the ordering.
	testutil.AddRemote(t, tmpDir, "first-real", "https://example.com/first.git")
	testutil.AddRemote(t, tmpDir, "second-real", "https://example.com/second.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "first-real", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
// Regression guard for the tracking tier that was removed before merge: the
// branch's tracking config must NOT decide the election.
//
// Election is compared against the remote of the push being made, so electing
// the tracking remote turns every push to a different remote into a silent
// no-op — the failure TestAlternates_RelativeObjectAlternate_CheckpointSync
// caught (clone with `-o base`, push checkpoints to a separately added
// origin). It also elects a remote the read paths cannot see, since resume and
// explain resolve checkpoints through origin's remote-tracking refs.
//
// The fork setup this tier was meant to serve (origin unpushable, push to your
// own fork) is served explicitly by checkpoint_push_remote, covered above.
func TestResolveCheckpointSyncRemote_TrackingConfigDoesNotDecide(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		keys map[string]string
	}{
		{"branch.<name>.remote", map[string]string{"branch.%s.remote": "upstream"}},
		{"remote.pushDefault", map[string]string{"remote.pushDefault": "upstream"}},
		{"branch.<name>.pushRemote", map[string]string{"branch.%s.pushRemote": "upstream"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			testutil.InitRepo(t, tmpDir)
			testutil.WriteFile(t, tmpDir, "f.txt", "init")
			testutil.GitAdd(t, tmpDir, "f.txt")
			testutil.GitCommit(t, tmpDir, "init")

			testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
			testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

			branch := currentBranchName(t, tmpDir)
			for key, val := range tt.keys {
				if strings.Contains(key, "%s") {
					key = fmt.Sprintf(key, branch)
				}
				setGitConfig(t, tmpDir, key, val)
			}

			t.Chdir(tmpDir)

			got, err := ResolveCheckpointSyncRemote(ctx)
			require.NoError(t, err)
			assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got,
				"tracking config must not outrank origin")
		})
	}
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingBeatsTracking(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")

	branch := currentBranchName(t, tmpDir)
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "upstream")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestCheckpointSyncAllowedForRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("no setting: allowed only for the elected default remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

		t.Chdir(tmpDir)

		assert.True(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("misconfigured setting fails closed for every remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")
		testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("unreadable settings fails closed for every remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

		// Corrupt settings.json, not a misconfigured setting: the gate must
		// fail closed here too, not just when the resolver itself detects a
		// bad checkpoint_push_remote value.
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("raw URL push argument is never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "https://github.com/o/r.git"))
	})

	t.Run("no remotes configured: never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
	})
}

// Granted-at-the-prompt seam (unreachable from the integration test): the
// SAME PrePush call must re-evaluate the gate and sync. Not parallel.
func TestPrePush_TrustGrantedAtPromptSyncsInSameCall(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	testutil.AddRemote(t, workDir, "origin", bareDir)
	t.Chdir(workDir)
	t.Setenv(settings.EnvCheckpointsPrimary, checkpoint.BackendTypeGitRefs)
	enrollRepoGlobally(t, `{"global":{"enabled":true}}`)
	prompted := 0
	oldResolve := resolveTrustDecisionFn
	resolveTrustDecisionFn = func(ctx context.Context, _ io.Writer) (TrustDecision, error) {
		prompted++
		_, err := settings.TrustCurrentRepo(ctx)
		require.NoError(t, err)
		return TrustGranted, nil
	}
	t.Cleanup(func() { resolveTrustDecisionFn = oldResolve })

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	enqueueRefs(t, repo, refs)
	stderr := captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePush(context.Background(), "origin"))

	require.Equal(t, 1, prompted, "the closed gate must consult the resolver exactly once")
	require.NotContains(t, stderr.String(), heldMessageFragment)
	remoteRefs := lsRemoteOutput(t, bareDir)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String(), "consent at the prompt must sync in that same PrePush call")
	}
}
