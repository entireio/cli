package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// TestXorbStore_ListedAndDeletedByCleanAll covers the reclaim path for
// git-remote-entire's xorb store. It holds whole xorbs of pushed large blobs,
// and nothing else removes the directory.
//
// Not parallel: t.Chdir sets process-global state.
func TestXorbStore_ListedAndDeletedByCleanAll(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()

	items, err := ListAllItems(ctx)
	require.NoError(t, err)
	require.NotContains(t, cleanupItemTypes(items), CleanupTypeXorbStore,
		"an absent store must not be listed")

	commonDir, err := session.GetGitCommonDir(ctx)
	require.NoError(t, err)
	dir := filepath.Join(commonDir, gitdir.XorbStoreDirName)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "0123abcd", "xorbs"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "0123abcd", "xorbs", "x"), []byte("xorb"), 0o600))

	items, err = ListAllItems(ctx)
	require.NoError(t, err)
	require.Contains(t, cleanupItemTypes(items), CleanupTypeXorbStore)

	result, err := DeleteAllCleanupItems(ctx, items)
	require.NoError(t, err)
	require.Equal(t, []string{gitdir.XorbStoreDirName}, result.XorbStores)
	require.Empty(t, result.FailedXorbStore)
	require.NoDirExists(t, dir)

	// Idempotent: a store that is already gone is not an error.
	require.NoError(t, deleteXorbStore(ctx))
}
