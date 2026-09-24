package strategy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestStagedBlobs_UnusualExistingPathIsNotDeletion(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	name := "café.txt"
	testutil.WriteFile(t, dir, name, "before\n")
	testutil.GitAdd(t, dir, name)
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, name, "after\n")
	testutil.GitAdd(t, dir, name)
	t.Chdir(dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()

	staged, err := stagedBlobs(context.Background(), repo)
	require.NoError(t, err)
	entry, ok := staged[name]
	require.True(t, ok, "staged path must retain its literal bytes: %#v", staged)
	require.False(t, entry.deleted)
	require.NotZero(t, entry.blob)
}
