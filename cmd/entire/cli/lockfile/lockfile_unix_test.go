//go:build unix

package lockfile_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquire_ProcessExitReleases verifies kernel auto-release on
// process death. The child re-execs this binary with role="child", which
// acquires the lock and exits — the kernel releases the flock when the
// FD closes. The parent then re-acquires the same path.
func TestAcquire_ProcessExitReleases(t *testing.T) {
	t.Parallel()

	if os.Getenv("LOCKFILE_TEST_CHILD") == "1" {
		path := os.Getenv("LOCKFILE_TEST_PATH")
		_, err := lockfile.Acquire(path)
		if err != nil {
			os.Stderr.WriteString("CHILD_FAIL: " + err.Error())
			os.Exit(1)
		}
		os.Stderr.WriteString("CHILD_ACQUIRED")
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "test.lock")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAcquire_ProcessExitReleases", "-test.v")
	cmd.Env = append(os.Environ(),
		"LOCKFILE_TEST_CHILD=1",
		"LOCKFILE_TEST_PATH="+path,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "child failed: %s", out)
	require.Contains(t, string(out), "CHILD_ACQUIRED", "child must have acquired; got: %s", out)

	lk, err := lockfile.Acquire(path)
	require.NoError(t, err, "parent must acquire after child exit; child output: %s", out)
	t.Cleanup(func() { _ = lk.Release() }) //nolint:errcheck // test cleanup

	assert.Equal(t, os.Getpid(), lockfile.ReadHolderPID(path),
		"PID file should now hold parent's PID, not child's")
}

// TestAcquire_PermissionDenied is Unix-only because it uses os.Getuid
// and chmod-based permission semantics that don't translate to Windows.
func TestAcquire_PermissionDenied(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))       // r-x only
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:errcheck // test cleanup; chmod failure here only affects t.TempDir's own cleanup

	path := filepath.Join(dir, "test.lock")
	lk, err := lockfile.Acquire(path)
	assert.Nil(t, lk)
	require.Error(t, err)
	assert.NotErrorIs(t, err, lockfile.ErrLocked, "permission errors must NOT be ErrLocked")
}
