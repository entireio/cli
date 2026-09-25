package strategy

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestLocalRefReads_NoGitAfterResolution(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/topic", head)
	testutil.RunGit(t, root, "symbolic-ref", "refs/remotes/origin/dangling", "refs/remotes/origin/absent")
	testutil.RunGit(t, root, "pack-refs", "--all")
	_, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, branchExistsFresh(t.Context(), "shadow"))
	require.Error(t, branchExistsFresh(t.Context(), "absent"))
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	require.False(t, remoteHasTrackingRefs(t.Context(), "other"))
}

func TestBranchExistsFresh_ObservesPackedDeletion(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	testutil.RunGit(t, root, "pack-refs", "--all")
	require.NoError(t, branchExistsFresh(t.Context(), "shadow"))
	testutil.RunGit(t, root, "branch", "-D", "shadow")
	require.Error(t, branchExistsFresh(t.Context(), "shadow"))
}

func TestLocalRefReads_StoreOverrideAndBare(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	other := t.TempDir()
	testutil.InitRepo(t, other)
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/topic", head)
	bare := filepath.Join(t.TempDir(), "bare.git")
	testutil.RunGit(t, root, "clone", "--mirror", root, bare)
	t.Chdir(bare)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	require.NoError(t, branchExistsFresh(t.Context(), "shadow"))
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	t.Chdir(other)
	paths.ClearWorktreeRootCache()
	require.Error(t, branchExistsFresh(t.Context(), "shadow"))
	require.False(t, remoteHasTrackingRefs(t.Context(), "origin"))
	// Keep the warmed root cache while changing the native repository selector.
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_WORK_TREE", root)
	require.NoError(t, branchExistsFresh(t.Context(), "shadow"))
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
}
