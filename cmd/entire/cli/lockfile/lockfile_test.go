package lockfile_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquire_FreshFileSucceeds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")

	lk, err := lockfile.Acquire(path)
	require.NoError(t, err)
	require.NotNil(t, lk)
	t.Cleanup(func() { _ = lk.Release() }) //nolint:errcheck // test cleanup

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "lock file must be 0600")
	}

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	pid, parseErr := strconv.Atoi(string(bytes.TrimSpace(data)))
	require.NoError(t, parseErr)
	assert.Equal(t, os.Getpid(), pid)
}

func TestAcquire_AlreadyHeldReturnsErrLocked(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")

	lk1, err := lockfile.Acquire(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk1.Release() }) //nolint:errcheck // test cleanup

	lk2, err := lockfile.Acquire(path)
	assert.Nil(t, lk2)
	require.Error(t, err)
	require.ErrorIs(t, err, lockfile.ErrLocked, "expected ErrLocked, got %v", err)

	// First holder's PID is still present — second attempt must NOT clobber it.
	assert.Equal(t, os.Getpid(), lockfile.ReadHolderPID(path))
}

func TestRelease_PermitsReacquire(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")

	lk1, err := lockfile.Acquire(path)
	require.NoError(t, err)
	require.NoError(t, lk1.Release())

	lk2, err := lockfile.Acquire(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk2.Release() }) //nolint:errcheck // test cleanup
}

func TestWithTimeout_RunsCallback(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	called := false

	err := lockfile.WithTimeout(context.Background(), path, time.Second, func() error {
		called = true
		assert.Equal(t, os.Getpid(), lockfile.ReadHolderPID(path))
		return nil
	})

	require.NoError(t, err)
	assert.True(t, called)
}

func TestWithTimeout_ReturnsErrLockedOnTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	lk, err := lockfile.Acquire(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() }) //nolint:errcheck // test cleanup

	err = lockfile.WithTimeout(context.Background(), path, time.Nanosecond, func() error {
		t.Fatal("callback should not run")
		return nil
	})

	require.ErrorIs(t, err, lockfile.ErrLocked)
}

func TestReadHolderPID_EmptyFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	require.NoError(t, os.WriteFile(path, []byte{}, 0o600))
	assert.Equal(t, 0, lockfile.ReadHolderPID(path))
}

func TestReadHolderPID_GarbageContent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	require.NoError(t, os.WriteFile(path, []byte("not a number"), 0o600))
	assert.Equal(t, 0, lockfile.ReadHolderPID(path))
}

func TestReadHolderPID_MissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.lock")
	assert.Equal(t, 0, lockfile.ReadHolderPID(path))
}
