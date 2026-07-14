//go:build unix

// Package flock provides a small cross-process advisory-lock primitive built
// on POSIX flock (Unix) / LockFileEx (Windows). It exists so that checkpoint
// and strategy can both serialize on shared resources without one taking
// the other as an import dependency.
package flock

import (
	"fmt"
	"os"
	"syscall"
)

// Acquire takes an exclusive advisory lock on path, creating the file if
// needed. The returned release closes the file, which drops the flock.
// Callers must invoke release exactly once. The lock file persists between
// runs — flock state is held by the file descriptor, not by the inode on
// disk — so the lockfile contents are immaterial.
func Acquire(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // file descriptors are non-negative; standard Go pattern for syscall.Flock
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
