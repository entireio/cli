package strategy

import (
	"context"
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestFlushCheckpointRefsQueue_NonTTY_NoProgressOutput verifies the git-refs
// default pre-push path stays silent on a non-TTY writer while still landing
// the queued refs. captureStderr's os.Pipe write end is never a terminal, so
// interactive.IsTerminalWriter(os.Stderr) is false for the duration of the
// call without any extra faking.
//
// Not parallel: uses captureStderr's os.Stderr redirection and t.Chdir.
func TestFlushCheckpointRefsQueue_NonTTY_NoProgressOutput(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	output := restore()

	require.NoError(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, len(refs), pushed)

	assert.NotContains(t, output, "[entire] Pushing", "non-TTY stderr must contain no progress banner")
	assert.NotContains(t, output, "syncing", "non-TTY stderr must contain no progress text")

	for _, ref := range refs {
		assert.NotEmpty(t, remoteRefHash(t, bareDir, ref), "ref should still land on the remote")
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "pushed refs are removed from the queue")
}
