package gitdir

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// Not parallel: t.Setenv.
func TestCommonDirForForeignWorktree_IgnoresInheritedRepoSelectors(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	testutil.InitRepo(t, repoA)
	testutil.InitRepo(t, repoB)

	// What git exports to a hook running in repo B.
	t.Setenv("GIT_DIR", filepath.Join(repoB, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(repoB, ".git", "index"))

	got, err := CommonDirForForeignWorktree(context.Background(), repoA)
	require.NoError(t, err)

	want, err := filepath.EvalSymlinks(filepath.Join(repoA, ".git"))
	require.NoError(t, err)
	gotResolved, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	require.Equal(t, want, gotResolved)
}
