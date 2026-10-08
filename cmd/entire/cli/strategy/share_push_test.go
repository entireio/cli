package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole point of sharing is that the checkpoint leaves this machine: a
// recipient resolves it by ID and fetches the ref. If it never reaches the
// remote, the resume command printed for them cannot work.
func TestPushSharedCheckpoints_DeliversRefsToTheRemote(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	result, err := (&ManualCommitStrategy{}).PushSharedCheckpoints(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.False(t, result.PushDisabled)
	assert.Equal(t, len(refs), result.Pushed)

	for _, ref := range refs {
		assert.NotEmpty(t, remoteRefHash(t, bareDir, ref), "a shared checkpoint must be on the remote")
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "delivered refs leave the queue")
}

// push_sessions=false must be reported as such, not as a successful share:
// the checkpoint stays local and nobody else can resume it.
func TestPushSharedCheckpoints_ReportsPushDisabled(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	require.NoError(t, os.MkdirAll(filepath.Join(workDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"push_sessions": false}}`),
		0o600,
	))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	result, err := (&ManualCommitStrategy{}).PushSharedCheckpoints(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.True(t, result.PushDisabled)
	assert.Equal(t, 0, result.Pushed)

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "a disabled push leaves the refs queued")
}

// A failed delivery must surface as an error rather than a quiet success:
// share is a foreground command whose whole job is getting the checkpoint out.
func TestPushSharedCheckpoints_FailureIsSurfaced(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	_, err = (&ManualCommitStrategy{}).PushSharedCheckpoints(
		context.Background(), repo, filepath.Join(t.TempDir(), "no-such-remote"))
	require.Error(t, err, "an unreachable remote must not read as a successful share")

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "a failed push leaves the refs queued for the next attempt")
}

// The git-branch backend must report delivery as strictly as git-refs. PrePush
// is fail-soft and returns nil when the sync gate skips the push or the remote
// refuses every ref; routing share through it reported those as a successful
// share and printed a resume command for a checkpoint that never left.
func TestPushSharedCheckpoints_GitBranchFailureIsNotSuccess(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-branch")
	workDir, _, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	result, err := (&ManualCommitStrategy{}).PushSharedCheckpoints(
		context.Background(), repo, filepath.Join(t.TempDir(), "no-such-remote"))
	require.Error(t, err, "an undeliverable branch push must not read as a successful share")
	assert.Equal(t, 0, result.Pushed)
}
