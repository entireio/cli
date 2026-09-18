package gitdir_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestCurrentWorktreeRootLifetimeAndMetadataRepair(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	gitdir.Reset()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(gitdir.Reset)
	root, err := gitdir.OpenForCurrentWorktree(t.Context())
	require.NoError(t, err)
	require.NoError(t, osroot.WriteFile(root, "proof", []byte("preserved"), 0o600))
	shared, err := gitdir.OpenAt(root.Name())
	require.NoError(t, err)
	require.Same(t, root, shared)
	confined, name, err := gitdir.OpenPathIn(root.Name(), filepath.Join(root.Name(), "proof"))
	require.NoError(t, err)
	require.Same(t, root, confined)
	require.Equal(t, "proof", name)
	_, _, err = gitdir.OpenPathIn(root.Name(), filepath.Join(dir, "outside"))
	require.Error(t, err)

	badPointer := filepath.Join(dir, ".git", "commondir")
	require.NoError(t, os.WriteFile(badPointer, []byte("missing\n"), 0o600))
	unavailable, err := gitdir.OpenForCurrentWorktree(t.Context())
	require.Error(t, err)
	require.Nil(t, unavailable)
	require.NoError(t, os.Remove(badPointer))
	repaired, err := gitdir.OpenForCurrentWorktree(t.Context())
	require.NoError(t, err)
	require.Same(t, root, repaired, "metadata repair must preserve registry-owned handles")

	gitdir.Reset()
	_, err = root.Stat("proof")
	require.Error(t, err, "Reset must close the old handle")
	reopened, err := gitdir.OpenForCurrentWorktree(t.Context())
	require.NoError(t, err)
	require.NotSame(t, root, reopened)
	content, err := osroot.ReadFile(reopened, "proof")
	require.NoError(t, err)
	require.Equal(t, "preserved", string(content))
}
