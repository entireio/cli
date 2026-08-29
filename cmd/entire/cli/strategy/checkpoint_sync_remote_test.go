package strategy

import (
	"bytes"
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
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

		assert.True(t, checkpointSyncAllowedForRemote(ctx, "origin", ""))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish", ""))
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

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin", ""))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish", ""))
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

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin", ""))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish", ""))
	})

	t.Run("raw URL push argument is never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "https://github.com/o/r.git", ""))
	})

	t.Run("no remotes configured: never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin", ""))
	})
}

// newCaptureTestRepo builds a repo with one commit and an origin+fork remote
// pair — the fork topology: origin wins the default election, fork is where
// the user's branches actually push.
// gitInRepo runs a git subcommand in repoDir with isolated config, for setup the
// testutil helpers do not cover (remote rename).
func gitInRepo(t *testing.T, repoDir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func newCaptureTestRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "fork", "https://example.com/fork.git")
	return tmpDir
}

// captureStderrWriter redirects the strategy package's user-facing stderr into
// a buffer for the duration of the test.
func captureStderrWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })
	return &buf
}

// Regression: after the single-remote gate (ENT-1451) shipped, a push to a
// non-elected remote stopped carrying checkpoints with no signal in the push
// output — users whose habitual push remote lost the default election to a
// dormant origin saw checkpoints strand locally and had to discover
// checkpoint_push_remote through support. The gated pre-push must name the
// elected destination and the setting.
//
// Not parallel: uses t.Chdir()
func TestHintGatedCheckpointSync(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	// initHintRepo builds a repo with unpushed v1 checkpoint commits and an
	// origin+publish remote pair, so a push to "publish" is gated under the
	// default election.
	// declared: whether the branch declares "publish" as its push destination.
	// The hint only speaks for a remote the branch actually pushes to, so the
	// declaration is part of the fixture rather than incidental.
	initHintRepo := func(t *testing.T, declared bool) string {
		dir, _, second := initCountTestRepo(t)
		testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, dir, "publish", "https://example.com/publish.git")
		testutil.GitUpdateRef(t, dir, v1LocalRef, second)
		if declared {
			setGitConfig(t, dir, "remote.pushDefault", "publish")
		}
		return dir
	}

	t.Run("automatic election with waiting checkpoints prints the hint", func(t *testing.T) {
		dir := initHintRepo(t, true)
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		out := buf.String()
		assert.Contains(t, out, `"origin"`, "hint must name the elected destination")
		assert.Contains(t, out, "checkpoint_push_remote", "hint must name the setting that re-routes sync")
		assert.Contains(t, out, `"publish"`, "hint must name the remote the user actually pushed")
		// A remote name is a per-clone fact: pointing at the tracked
		// settings.json would invite committing it, fail-closing sync for
		// every teammate whose clone lacks that remote name.
		assert.Contains(t, out, ".entire/settings.local.json", "hint must point at the clone-local settings file")
	})

	t.Run("a push to a remote this branch does not push to stays silent", func(t *testing.T) {
		// The advice is "point checkpoint_push_remote at the remote you just
		// pushed". For a deploy target or a one-off `git push upstream` that would
		// tell the user to publish session transcripts there — the leak the
		// single-remote gate exists to prevent. Silence is the only correct output.
		dir := initHintRepo(t, false)
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		assert.Empty(t, buf.String(),
			"an undeclared push target must not be recommended as the checkpoint sync remote")
	})

	t.Run("checkpoints already on the elected remote stay silent", func(t *testing.T) {
		// Pins that the count runs against the ELECTED remote, not the pushed
		// one: origin's tracking ref is up to date, publish has none, so
		// counting against "publish" would report every v1 commit and nag on
		// every gated push forever after checkpoints already synced.
		dir := initHintRepo(t, true)
		testutil.GitUpdateRef(t, dir, "refs/remotes/origin/"+paths.MetadataBranchName, testutil.GetHeadHash(t, dir))
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		assert.Empty(t, buf.String(), "nothing is waiting for the elected remote; the gated push has nothing to warn about")
	})

	t.Run("explicit checkpoint_push_remote stays silent", func(t *testing.T) {
		dir := initHintRepo(t, true)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "origin")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		assert.Empty(t, buf.String(), "a configured election is a decision already made; gated pushes must not nag")
	})

	t.Run("nothing waiting stays silent", func(t *testing.T) {
		dir, _, _ := initCountTestRepo(t)
		testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, dir, "publish", "https://example.com/publish.git")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		assert.Empty(t, buf.String(), "no stranded checkpoints, nothing to warn about")
	})

	t.Run("raw URL push stays silent", func(t *testing.T) {
		dir := initHintRepo(t, true)
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "https://example.com/elsewhere.git")

		assert.Empty(t, buf.String(), "checkpoint_push_remote takes a remote name; a URL push has no actionable hint")
	})

	t.Run("failed election stays silent", func(t *testing.T) {
		dir := initHintRepo(t, true)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "gone")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		hintGatedCheckpointSync(ctx, "publish")

		assert.Empty(t, buf.String(), "the fail-closed election already logs its own warning")
	})
}

// Regression: the default election guesses from config at rest (origin bias),
// so a user whose branches push a non-origin remote had checkpoints strand
// locally until they hand-configured checkpoint_push_remote (first user report
// the day after v0.10.0 shipped ENT-1451). A captured election — written when
// an actual push agrees with the branch's declared push destination — must
// outrank origin, while an explicit setting still outranks the capture.
//
// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_CapturedTier(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("captured remote beats origin", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceObserved}, got)
	})

	t.Run("explicit checkpoint_push_remote beats captured", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "origin")
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceConfig}, got)
	})

	t.Run("captured remote no longer configured falls through to origin", func(t *testing.T) {
		// Fail-soft, unlike the fail-closed explicit setting: capture is
		// automatic, so a renamed/removed remote must not disable sync.
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "gone"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
	})

	t.Run("corrupt capture state falls through to origin", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		path, err := capturedSyncRemotesPath(ctx)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

		got, resolveErr := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, resolveErr)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
	})
}

// captureOnSuccessfulPush runs both capture phases in the order a pre-push that
// delivers checkpoints runs them: propose before the push, persist after it. The
// subtests below assert that end state, which is unchanged by the split;
// TestCaptureCheckpointSyncRemote_OnlyOnDelivery covers the split itself.
func captureOnSuccessfulPush(ctx context.Context, pushRemote string) {
	if pendingCaptureCheckpointSyncRemote(ctx, pushRemote) {
		commitCapturedSyncRemote(ctx, pushRemote)
	}
}

// Not parallel: uses t.Chdir()
func TestCaptureCheckpointSyncRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("push agreeing with the branch's declared destination captures and announces", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "capture must persist the remote")
		assert.Contains(t, buf.String(), `"fork"`, "capture must announce itself")
		assert.Contains(t, buf.String(), "checkpoint_push_remote", "announcement must name the override")
		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceObserved}, got)
	})

	t.Run("push to the already-elected remote does not capture", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "origin")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "origin")

		assert.Empty(t, loadCapturedSyncRemotes(ctx), "the seed election needs no capture; persisting it would block a later real capture")
		assert.Empty(t, buf.String())
	})

	t.Run("one-off push to a non-declared remote does not capture", func(t *testing.T) {
		// The upstream-PR push: behavior without declaration is not consent.
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "origin")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "fork")

		assert.Empty(t, loadCapturedSyncRemotes(ctx))
		assert.Empty(t, buf.String())
	})

	t.Run("existing capture sticks", func(t *testing.T) {
		// Phase-1 no-ping-pong rule: a mixed-habit repo (branches pushing two
		// remotes) must not flip the election back and forth on every push.
		dir := newCaptureTestRepo(t)
		testutil.AddRemote(t, dir, "work", "https://example.com/work.git")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "work")
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "work")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "first capture sticks until config or phase 2")
		assert.Empty(t, buf.String())
	})

	t.Run("a dead capture does not block a fresh one", func(t *testing.T) {
		// The sticky rule asks whether a capture is IN FORCE, not whether the file
		// is non-empty. Gating on presence stranded the election: after
		// `git remote rename fork myfork` the resolver skipped the dead entry and
		// fell back to origin, while capture refused to ever elect myfork — so for
		// a user who only pushes to myfork every push was gated and checkpoints
		// reached nowhere, recoverable only by deleting the state file by hand.
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))
		// The rename is what kills the captured entry: fork stops existing, the
		// state file still names it, and myfork becomes the declared destination.
		gitInRepo(t, dir, "remote", "rename", "fork", "myfork")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "myfork")
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "myfork")

		assert.Equal(t, []string{"myfork"}, loadCapturedSyncRemotes(ctx),
			"a captured remote that no longer exists must not veto the next capture")
		assert.Contains(t, buf.String(), `"myfork"`, "the new election must be announced")
	})

	t.Run("explicit checkpoint_push_remote disables capture", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "origin")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "fork")

		assert.Empty(t, loadCapturedSyncRemotes(ctx), "an explicit setting is a decision already made")
		assert.Empty(t, buf.String())
	})

	t.Run("branch pushRemote outranks branch remote in the declaration", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		branch := currentBranchName(t, dir)
		setGitConfig(t, dir, "branch."+branch+".remote", "origin")
		setGitConfig(t, dir, "branch."+branch+".pushRemote", "fork")
		t.Chdir(dir)

		captureOnSuccessfulPush(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "pushRemote is git's own push-resolution winner")
	})

	t.Run("remote.pushDefault declares when the branch has no tracking", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "remote.pushDefault", "fork")
		t.Chdir(dir)

		captureOnSuccessfulPush(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx))
	})

	t.Run("raw URL push never captures", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		captureOnSuccessfulPush(ctx, "https://example.com/elsewhere.git")

		assert.Empty(t, loadCapturedSyncRemotes(ctx))
		assert.Empty(t, buf.String())
	})
}

// The election is permanent and one-shot ("first capture sticks"), so it must
// follow evidence that checkpoints ARRIVED, not evidence that a push was about
// to be attempted. Everything between the gate and the network can still stop
// delivery — a diverged checkpoint policy, an OPF rewrite failure, the
// empty-remote defer, a rejected transfer — and capturing on intent both
// announced a move that carried nothing and left the queued checkpoints able to
// drain only to the remote that had just failed to take them.
//
// Not parallel: uses t.Chdir()
func TestCaptureCheckpointSyncRemote_OnlyOnDelivery(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("a push that never delivers leaves the election alone", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		// The pre-push proposes, then something downstream returns early and
		// commitCapturedSyncRemote is never reached.
		require.True(t, pendingCaptureCheckpointSyncRemote(ctx, "fork"),
			"fork is the declared destination, so this push would elect it")

		assert.Empty(t, loadCapturedSyncRemotes(ctx), "proposing must not write the election")
		assert.Empty(t, buf.String(), "nothing was delivered, so nothing may be announced")
		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got,
			"the election must still be recoverable by a later push that does deliver")
	})

	t.Run("the same push retried later still captures", func(t *testing.T) {
		// The corollary: declining to persist on a failed push must not poison
		// the next one, or a single transient failure would cost the user the
		// automatic election entirely.
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)

		require.True(t, pendingCaptureCheckpointSyncRemote(ctx, "fork"))
		captureOnSuccessfulPush(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx))
	})

	t.Run("commit re-checks eligibility after the push", func(t *testing.T) {
		// The lock is dropped across the network push, so another worktree's
		// hook can capture in between. The winner is whoever's state lands
		// first; the loser must not append itself on top.
		dir := newCaptureTestRepo(t)
		testutil.AddRemote(t, dir, "work", "https://example.com/work.git")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)

		require.True(t, pendingCaptureCheckpointSyncRemote(ctx, "fork"))
		// Another worktree delivers to work and commits first.
		require.NoError(t, saveCapturedSyncRemote(ctx, "work"))
		buf := captureStderrWriter(t)

		commitCapturedSyncRemote(ctx, "fork")

		assert.Equal(t, []string{"work"}, loadCapturedSyncRemotes(ctx),
			"a capture already in force wins; the stale proposal must not overwrite or append")
		assert.Empty(t, buf.String(), "a declined commit must not announce")
	})

	t.Run("the gate admits the push that will elect the remote", func(t *testing.T) {
		// Capture and the confinement gate have to agree, or the electing push
		// would be gated and there would be nothing delivered to capture on.
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "fork", ""),
			"without a pending capture fork is just a non-elected remote")
		assert.True(t, checkpointSyncAllowedForRemote(ctx, "fork", "fork"),
			"the push that elects fork must be allowed to carry checkpoints there")
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "other", "fork"),
			"a pending capture only admits the remote it names")
	})
}

// Granted-at-the-prompt seam (unreachable from the integration test): the
// SAME PrePush call must re-evaluate the gate and sync. Not parallel.
func TestPrePush_TrustGrantedAtPromptSyncsInSameCall(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	// No origin means trust is path-scoped. Use a separate delivery remote so
	// the hermetic bare path is not mistaken for an invalid configured origin.
	testutil.AddRemote(t, workDir, "sync", bareDir)
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

	require.NoError(t, NewManualCommitStrategy().PrePush(context.Background(), "sync"))

	require.Equal(t, 1, prompted, "the closed gate must consult the resolver exactly once")
	require.NotContains(t, stderr.String(), heldMessageFragment)
	remoteRefs := lsRemoteOutput(t, bareDir)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String(), "consent at the prompt must sync in that same PrePush call")
	}
}

// A push that would elect a new sync remote by evidence asks consent for THAT
// remote on the same push: origin's grant does not cover fork, the hold names
// `entire trust --remote fork`, nothing is captured because nothing was
// delivered, and trusting fork by the override releases both the checkpoints
// and the election in the next push.
// Not parallel: uses t.Chdir()
func TestPrePush_PendingCaptureAsksConsentForTheNewRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	testutil.AddRemote(t, workDir, "origin", "https://github.com/acme/widgets.git")
	testutil.AddRemote(t, workDir, "fork", bareDir)
	setGitConfig(t, workDir, "branch."+currentBranchName(t, workDir)+".remote", "fork")
	t.Chdir(workDir)
	t.Setenv(settings.EnvCheckpointsPrimary, checkpoint.BackendTypeGitRefs)
	enrollRepoGlobally(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
	prompted := 0
	oldResolve := resolveTrustDecisionFn
	resolveTrustDecisionFn = func(context.Context, io.Writer) (TrustDecision, error) {
		prompted++
		return TrustHeld, nil
	}
	t.Cleanup(func() { resolveTrustDecisionFn = oldResolve })
	ctx := context.Background()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)
	stderr := captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePush(ctx, "fork"))

	require.Equal(t, 1, prompted, "the closed gate consults the resolver once")
	assert.Contains(t, stderr.String(), "trusted for fork")
	assert.Contains(t, stderr.String(), "entire trust --remote fork")
	assert.Empty(t, loadCapturedSyncRemotes(ctx), "a held push delivered nothing, so it must not elect fork")
	assert.NotContains(t, lsRemoteOutput(t, bareDir), refs[0].String(), "held refs never leave the machine")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "held push leaves refs queued")
	enqueueRefs(t, repo, refs)

	// fork's URL is a bare path, so the override records a path key.
	_, err = settings.TrustCurrentRepo(WithSyncRemoteOverride(ctx, "fork"))
	require.NoError(t, err)
	stderr.Reset()
	require.NoError(t, NewManualCommitStrategy().PrePush(ctx, "fork"))

	assert.Equal(t, 1, prompted, "a trusted repo is not asked again")
	assert.NotContains(t, stderr.String(), heldMessageFragment)
	remoteRefs := lsRemoteOutput(t, bareDir)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String(), "trusting fork releases the queued checkpoints to it")
	}
	assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "delivery captures the election")
}

// A trusted repo keeps main's pre-push flow: the consent resolver is never
// consulted, whether or not anything is pending.
// Not parallel: uses t.Chdir()
func TestPrePush_TrustedRepoNeverConsultsResolver(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	testutil.AddRemote(t, workDir, "sync", bareDir)
	t.Chdir(workDir)
	t.Setenv(settings.EnvCheckpointsPrimary, checkpoint.BackendTypeGitRefs)
	enrollRepoGlobally(t, `{"global":{"enabled":true,"trust_all":true}}`)
	oldResolve := resolveTrustDecisionFn
	resolveTrustDecisionFn = func(context.Context, io.Writer) (TrustDecision, error) {
		t.Fatal("resolver consulted for a trusted repo")
		return TrustHeld, nil
	}
	t.Cleanup(func() { resolveTrustDecisionFn = oldResolve })

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	enqueueRefs(t, repo, refs)
	require.NoError(t, NewManualCommitStrategy().PrePush(context.Background(), "sync"))
	remoteRefs := lsRemoteOutput(t, bareDir)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String())
	}
	// Nothing pending now: still no prompt, still no error.
	require.NoError(t, NewManualCommitStrategy().PrePush(context.Background(), "sync"))
}
