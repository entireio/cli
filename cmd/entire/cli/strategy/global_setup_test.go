package strategy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// newGloballyTrackedRepo creates a git repo with no repo-level settings, makes
// it the cwd, and enables the user-global tier for it. Not parallel-safe
// (t.Chdir, t.Setenv).
func newGloballyTrackedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "hi\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	session.ClearGitCommonDirCache()
	t.Cleanup(session.ClearGitCommonDirCache)
	enrollRepoGlobally(t, `{"global":{"enabled":true}}`)
	return dir
}

func globalRuntimeRootForTest(t *testing.T) string {
	t.Helper()
	policy, err := repopolicy.ClassifyRepoPolicy(t.Context())
	require.NoError(t, err)
	require.Equal(t, repopolicy.ActivationGlobal, policy.ActivationSource, "fixture must be globally tracked")
	return policy.RuntimeRoot()
}

func TestMaybeEnsureGlobalSetup_InstallsHooksAndRefUnderGit(t *testing.T) {
	dir := newGloballyTrackedRepo(t)
	ctx := t.Context()

	MaybeEnsureGlobalSetup(ctx)

	require.True(t, IsGitHookInstalled(ctx), "git hooks installed by lazy setup")
	stamp := filepath.Join(globalRuntimeRootForTest(t), primaryRefStamp)
	_, err := os.Stat(stamp)
	require.NoError(t, err, "primary-ref stamp written under the git common dir")
	_, err = os.Lstat(filepath.Join(dir, ".entire"))
	require.True(t, os.IsNotExist(err), "global mode must not create a worktree .entire directory")

	// Idempotent: a second call rewrites nothing.
	hookPath := filepath.Join(dir, ".git", "hooks", "post-commit")
	before, err := os.Stat(hookPath)
	require.NoError(t, err)
	MaybeEnsureGlobalSetup(ctx)
	after, err := os.Stat(hookPath)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime(), "second run must not rewrite installed hooks")
}

func TestMaybeEnsureGlobalSetup_SkipsHooksWhenHooksPathIsInWorktree(t *testing.T) {
	dir := newGloballyTrackedRepo(t)
	ctx := t.Context()
	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	MaybeEnsureGlobalSetup(ctx)

	_, err = os.Lstat(filepath.Join(dir, ".husky"))
	require.True(t, os.IsNotExist(err), "a worktree-resident hooks dir must never be created")
	_, err = os.Lstat(filepath.Join(dir, ".entire"))
	require.True(t, os.IsNotExist(err), "global mode must not create a worktree .entire directory")
	_, err = os.Stat(filepath.Join(globalRuntimeRootForTest(t), primaryRefStamp))
	require.NoError(t, err, "the ref half of lazy setup still completes")
}

func TestMaybeEnsureGlobalSetup_ProbeErrorWritesNothing(t *testing.T) {
	dir := newGloballyTrackedRepo(t)
	ctx := t.Context()
	SetHooksDirProbeErrorForTesting(errors.New("boom"))
	t.Cleanup(func() { SetHooksDirProbeErrorForTesting(nil) })

	MaybeEnsureGlobalSetup(ctx)

	require.False(t, IsGitHookInstalled(ctx), "no hooks when the hooks dir cannot be resolved")
	_, err := os.Stat(filepath.Join(globalRuntimeRootForTest(t), primaryRefStamp))
	require.True(t, os.IsNotExist(err), "no stamp when setup bailed before the ref step")
	_, err = os.Lstat(filepath.Join(dir, ".entire"))
	require.True(t, os.IsNotExist(err))
}

func TestMaybeEnsureGlobalSetup_RejectsSymlinkedRuntimeParent(t *testing.T) {
	dir := newGloballyTrackedRepo(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".git", "entire")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	MaybeEnsureGlobalSetup(t.Context())

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "the primary-ref stamp must not be written through a planted symlink")
}

func TestMaybeEnsureGlobalSetup_NoopForRepoEnabledRepo(t *testing.T) {
	dir := newGloballyTrackedRepo(t)
	writeEnabledRepoSettings(t, dir)
	ctx := context.Background()

	MaybeEnsureGlobalSetup(ctx)

	require.False(t, IsGitHookInstalled(ctx), "repo-level activation is not the lazy path's job")
}
