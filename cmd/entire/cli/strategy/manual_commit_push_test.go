package strategy

import (
	"context"
	"os/exec"
	"testing"

	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkCheckpointSkipMessage(t *testing.T) {
	t.Parallel()

	assert.Empty(t, forkCheckpointSkipMessage(nil), "nil mismatch must produce no message")

	msg := forkCheckpointSkipMessage(&checkpointremote.ForkOwnerMismatchError{
		PushOwner:   "alice",
		TargetOwner: "acme",
		TargetRepo:  "acme/checkpoints",
	})
	assert.Contains(t, msg, "NOT pushed", "must state checkpoints were not pushed")
	assert.Contains(t, msg, "acme/checkpoints", "must name the configured private target")
	assert.Contains(t, msg, `"alice"`, "must name the fork owner")
	assert.Contains(t, msg, ".entire/settings.local.json", "must point at the local settings file")
	assert.Contains(t, msg, `"checkpoint_remote": null`, "must show the clear-target escape hatch")
	assert.Contains(t, msg, "point checkpoint_remote at your own private repo", "must show the repoint escape hatch")
}

// TestDeferCheckpointPushOnEmptyRemote_UsesLocalTrackingRefs verifies the guard
// decides purely from local remote-tracking refs, with no network access: a
// remote with no refs/remotes/<remote>/* is treated as possibly-empty (defer),
// and one with any tracking ref is treated as established (publish).
func TestDeferCheckpointPushOnEmptyRemote_UsesLocalTrackingRefs(t *testing.T) {
	// No t.Parallel: uses t.Chdir.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run(), "git %v", args)
	}
	run("commit", "--allow-empty", "-m", "init")
	// A deliberately unreachable URL: the guard must never dial it.
	run("remote", "add", "origin", "https://example.invalid/repo.git")

	t.Chdir(dir)
	ctx := context.Background()
	ps := pushSettings{remote: "origin"}

	// No remote-tracking refs yet → possibly a brand-new remote → defer.
	require.True(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"a remote with no tracking refs must defer")

	// A push straight to a bare URL is not a configured remote; git never records
	// a tracking ref for it, so the guard must publish rather than defer forever.
	require.False(t,
		deferCheckpointPushOnEmptyRemote(ctx, pushSettings{remote: "https://example.invalid/repo.git"}),
		"a bare-URL push target must not defer")

	// git records a remote-tracking ref after the first successful push; simulate
	// that locally (no network). The remote is now established → publish.
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	require.False(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"a remote with a tracking ref must not defer")

	// A configured separate checkpoint remote is always exempt.
	require.False(t,
		deferCheckpointPushOnEmptyRemote(ctx, pushSettings{remote: "origin", checkpointURL: "https://example.invalid/cp.git"}),
		"a dedicated checkpoint remote is exempt from the guard")
}
