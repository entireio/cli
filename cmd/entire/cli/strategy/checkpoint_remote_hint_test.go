package strategy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// hintRepo builds a repo whose committed settings name a checkpoint_remote,
// with origin's owner supplied by the caller: matching it is a store this
// developer owns, differing is one inherited by cloning.
func hintRepo(t *testing.T, originOwner string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:"+originOwner+"/main-repo.git")

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "acme/checkpoints"}}}`),
		0o644,
	))
	// Seed the local v1 branch so the adopted case does not dial github.com to
	// fetch a metadata branch it has never seen.
	testutil.RunGit(t, dir, "branch", paths.MetadataBranchName)
	return dir
}

// captureHintStderr redirects the writer the pre-push hints print to.
func captureHintStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := stderrWriter
	var buf bytes.Buffer
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = old })
	return &buf
}

// TestPrePushNamesTheCommandThatClaimsAnIgnoredCheckpointRemote is the point of
// the warning: before it, the ownership rejection reached the user only as a
// Warn in .entire/logs, and the visible symptom — checkpoints landing in the
// code repository — looks like a working setup.
//
// Not parallel: t.Chdir.
func TestPrePushNamesTheCommandThatClaimsAnIgnoredCheckpointRemote(t *testing.T) {
	dir := hintRepo(t, "alice")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	out := captureHintStderr(t)

	ctx := context.Background()
	ps := resolvePushSettings(ctx, "origin")
	require.False(t, ps.hasCheckpointURL(), "fixture must be the rejected case")
	warnIgnoredCheckpointRemote(ctx, ps)

	got := out.String()
	assert.Contains(t, got, "acme/checkpoints", "the store the user configured is named")
	assert.Contains(t, got, `"origin"`, "so is the store their checkpoints are actually going to")
	assert.Contains(t, got, "entire enable --local --checkpoint-remote github:acme/checkpoints",
		"the remedy is a command to run, not a file to go and edit")
}

// TestPrePushStripsTerminalEscapesFromTheCheckpointRemote: the repo comes from
// the committed settings file, so it must not be able to drive the terminal.
//
// Not parallel: t.Chdir.
func TestPrePushStripsTerminalEscapesFromTheCheckpointRemote(t *testing.T) {
	dir := hintRepo(t, "alice")
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "acme/store\u001b[2J\u001b]0;pwned\u0007"}}}`),
		0o644,
	))
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	out := captureHintStderr(t)

	ctx := context.Background()
	warnIgnoredCheckpointRemote(ctx, resolvePushSettings(ctx, "origin"))

	got := out.String()
	assert.Contains(t, got, "acme/store")
	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\a")
}

// TestPrePushSaysNothingWhenTheCheckpointRemoteIsInUse is the control: the
// warning prints on every push while the condition holds, so a false positive
// is noise on every push of a correctly configured repo.
//
// Not parallel: t.Chdir.
func TestPrePushSaysNothingWhenTheCheckpointRemoteIsInUse(t *testing.T) {
	dir := hintRepo(t, "acme")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	out := captureHintStderr(t)

	ctx := context.Background()
	ps := resolvePushSettings(ctx, "origin")
	require.True(t, ps.hasCheckpointURL(), "fixture must be the adopted case")
	warnIgnoredCheckpointRemote(ctx, ps)

	assert.Empty(t, out.String(), "a store that is in use has nothing to report")
}

// TestPrePushSaysNothingWithoutACheckpointRemote pins the other half of the
// free gate: an empty checkpointURL means "none configured" as often as
// "configured and refused", and only the second is worth a line.
//
// Not parallel: t.Chdir.
func TestPrePushSaysNothingWithoutACheckpointRemote(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/main-repo.git")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	out := captureHintStderr(t)

	ctx := context.Background()
	ps := resolvePushSettings(ctx, "origin")
	require.False(t, ps.checkpointRemoteConfigured)
	warnIgnoredCheckpointRemote(ctx, ps)

	assert.Empty(t, out.String(), "no checkpoint_remote, nothing ignored")
}

// TestPrePushSurfacesTheIgnoredCheckpointRemoteToTheUser proves the warning is
// wired into the push the user actually runs, not merely callable. Origin is a
// local bare repo here, which is itself one of the rejection reasons — a path
// URL has no owner to compare — and keeps the push off the network.
//
// Not parallel: t.Chdir.
func TestPrePushSurfacesTheIgnoredCheckpointRemoteToTheUser(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	bare := filepath.Join(t.TempDir(), "origin.git")
	_, err := git.PlainInit(bare, true)
	require.NoError(t, err)
	testutil.RunGit(t, dir, "remote", "add", "origin", bare)

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "acme/checkpoints"}}}`),
		0o644,
	))
	testutil.RunGit(t, dir, "branch", paths.MetadataBranchName)

	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	out := captureHintStderr(t)

	require.NoError(t, NewManualCommitStrategy().PrePush(context.Background(), "origin"))

	assert.Contains(t, out.String(), "entire enable --local --checkpoint-remote github:acme/checkpoints",
		"a user pushing must see the fix in their own push output")
}
