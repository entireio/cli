package strategy

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestLocalRefReads_StoreOverrideAndBare(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	other := t.TempDir()
	testutil.InitRepo(t, other)
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/topic", head)
	bare := filepath.Join(t.TempDir(), "bare.git")
	testutil.RunGit(t, root, "clone", "--mirror", root, bare)
	t.Chdir(bare)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	t.Chdir(other)
	paths.ClearWorktreeRootCache()
	require.False(t, remoteHasTrackingRefs(t.Context(), "origin"))
	// Keep the warmed root cache while changing the native repository selector.
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_WORK_TREE", root)
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
}
