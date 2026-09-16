package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// lockDir returns <git-common-dir>/<name>, creating it.
func lockDir(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	commonDir, err := session.GetGitCommonDir(ctx)
	require.NoError(t, err)
	dir := filepath.Join(commonDir, name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	return dir
}

// TestRefLocks_AbsentDirsAreNotListed keeps `entire clean --all` quiet when a
// repo has never taken a checkpoint lock -- an empty or missing lock directory
// is nothing to offer.
//
// Not parallel: t.Chdir sets process-global state.
func TestRefLocks_AbsentDirsAreNotListed(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	items, err := ListAllItems(context.Background())
	require.NoError(t, err)
	require.NotContains(t, cleanupItemTypes(items), CleanupTypeRefLock,
		"no lock directory exists, so none must be listed")
}

// TestRefLocks_UnheldFilesAreReapedByCleanAll covers the growth path from the
// issue: entire-persistent-ref-locks accrues one file per checkpoint ref and
// nothing else removes them.
//
// Not parallel: t.Chdir sets process-global state.
func TestRefLocks_UnheldFilesAreReapedByCleanAll(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()
	dir := lockDir(ctx, t, checkpoint.PersistentRefLockDirName)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "refs_entire_checkpoints_v1.lock"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "refs_entire_checkpoints_shard_0.lock"), nil, 0o600))

	items, err := ListAllItems(ctx)
	require.NoError(t, err)
	require.Contains(t, cleanupItemTypes(items), CleanupTypeRefLock,
		"a populated lock directory must be listed for cleanup")

	result, err := DeleteAllCleanupItems(ctx, items)
	require.NoError(t, err)
	require.Contains(t, result.RefLocks, checkpoint.PersistentRefLockDirName)
	require.Empty(t, result.FailedRefLocks)
	require.NoDirExists(t, dir, "an emptied lock directory is removed with its files")
}

// TestRefLocks_HeldFileSurvivesReap is the invariant that makes this safe to
// run while agents are working: a lock whose flock is currently held belongs to
// a live writer and must be left alone, even as its unheld siblings are reaped.
//
// Not parallel: t.Chdir sets process-global state.
func TestRefLocks_HeldFileSurvivesReap(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()
	dir := lockDir(ctx, t, checkpoint.ShadowLockDirName)
	heldPath := filepath.Join(dir, "entire_abc1234-def567.lock")
	unheldPath := filepath.Join(dir, "entire_0000000-000000.lock")
	require.NoError(t, os.WriteFile(heldPath, nil, 0o600))
	require.NoError(t, os.WriteFile(unheldPath, nil, 0o600))

	release, err := flock.Acquire(heldPath)
	require.NoError(t, err)
	defer release()

	items, err := ListAllItems(ctx)
	require.NoError(t, err)

	result, err := DeleteAllCleanupItems(ctx, items)
	require.NoError(t, err)
	require.Contains(t, result.RefLocks, checkpoint.ShadowLockDirName)
	require.Empty(t, result.FailedRefLocks)

	require.FileExists(t, heldPath, "a held lock file must survive the reap")
	require.NoFileExists(t, unheldPath, "an unheld sibling is still reaped")
	require.DirExists(t, dir, "the directory stays while it still holds a live lock")
}

// TestReapLockDirs_MissingDirIsNotAnError keeps the reap idempotent: a
// directory already gone (reaped by an earlier run, or never created) reports
// as reaped, not failed.
//
// Not parallel: t.Chdir sets process-global state.
func TestReapLockDirs_MissingDirIsNotAnError(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	reaped, failed := reapLockDirs(context.Background(), lockDirNames())
	require.ElementsMatch(t, lockDirNames(), reaped)
	require.Empty(t, failed)
}
