package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// TestRedactCache_ListedAndDeletedByCleanAll covers the reclaim path for the
// redaction prefix cache. It accumulates one small entry per session in the git
// common dir and is never superseded, so without being wired into the cleanup
// surface it would survive every `entire clean` indefinitely.
//
// Not parallel: t.Chdir sets process-global state.
func TestRedactCache_ListedAndDeletedByCleanAll(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()

	// Absent cache: nothing to offer.
	items, err := ListAllItems(ctx)
	require.NoError(t, err)
	require.NotContains(t, cleanupItemTypes(items), CleanupTypeRedactCache,
		"an absent cache directory must not be listed")

	// Seed an entry the way a checkpoint write would.
	dir, err := redactCacheDir(ctx)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deadbeef.json"),
		[]byte(`{"source_bytes":1}`), 0o600))

	items, err = ListAllItems(ctx)
	require.NoError(t, err)
	require.Contains(t, cleanupItemTypes(items), CleanupTypeRedactCache,
		"a populated cache directory must be listed for cleanup")

	result, err := DeleteAllCleanupItems(ctx, items)
	require.NoError(t, err)
	require.NotEmpty(t, result.RedactCaches)
	require.Empty(t, result.FailedRedactCache)
	require.NoDirExists(t, dir, "clean all must reclaim the cache directory")
}

// TestDeleteRedactCache_AbsentDirIsNotAnError keeps cleanup idempotent: removing
// a cache that was already reclaimed must not fail the rest of the run.
func TestDeleteRedactCache_AbsentDirIsNotAnError(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	require.NoError(t, deleteRedactCache(context.Background()))
}

func cleanupItemTypes(items []CleanupItem) []CleanupType {
	types := make([]CleanupType, 0, len(items))
	for _, item := range items {
		types = append(types, item.Type)
	}
	return types
}
