package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// TestLockFiles_ListedAndDeletedByCleanAll covers stale lock files in the git
// common dir. They are only coordination sentinels, so unlocked files should not
// survive `entire clean` indefinitely.
//
// Not parallel: t.Chdir sets process-global state.
func TestLockFiles_ListedAndDeletedByCleanAll(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()
	shadowLock := filepath.Join(tmpDir, ".git", "entire-shadow-locks", "entire_deadbeef.lock")
	persistentLock := filepath.Join(tmpDir, ".git", "entire-persistent-ref-locks", "refs_heads_entire_checkpoints_v1.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(shadowLock), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(persistentLock), 0o700))
	require.NoError(t, os.WriteFile(shadowLock, nil, 0o600))
	require.NoError(t, os.WriteFile(persistentLock, nil, 0o600))

	items, err := ListAllItems(ctx)
	require.NoError(t, err)
	require.Contains(t, cleanupItemTypes(items), CleanupTypeLockFile)

	result, err := DeleteAllCleanupItems(ctx, items)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		filepath.Join("entire-shadow-locks", "entire_deadbeef.lock"),
		filepath.Join("entire-persistent-ref-locks", "refs_heads_entire_checkpoints_v1.lock"),
	}, result.LockFiles)
	require.Empty(t, result.FailedLockFiles)
	require.NoFileExists(t, shadowLock)
	require.NoFileExists(t, persistentLock)
}

// TestDeleteCleanupLockFiles_HeldLockIsPreserved verifies that cleanup never
// unlinks a file while another process still holds the advisory lock.
func TestDeleteCleanupLockFiles_HeldLockIsPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	lockID := filepath.Join("entire-shadow-locks", "held.lock")
	lockPath := filepath.Join(tmpDir, ".git", lockID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o700))
	release, err := flock.Acquire(lockPath)
	require.NoError(t, err)
	defer release()

	deleted, skipped, failed, err := deleteCleanupLockFiles(context.Background(), []string{lockID})
	require.NoError(t, err)
	require.Empty(t, deleted)
	require.Equal(t, []string{lockID}, skipped)
	require.Empty(t, failed)
	require.FileExists(t, lockPath)
}
