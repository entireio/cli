//go:build unix

package strategy

import (
	"fmt"
	"os"
	"syscall"
)

// acquireStateFileLock takes an exclusive POSIX advisory lock on path. The
// returned release closes the file (which drops the flock). Callers must call
// release exactly once. The lock file persists between runs — that's fine,
// flock state is held by the file descriptor, not the inode on disk.
func acquireStateFileLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path built from validated session ID
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // file descriptors are non-negative; standard Go pattern for syscall.Flock
		_ = f.Close()
		return nil, fmt.Errorf("flock state lock: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
