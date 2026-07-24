//go:build unix

// Package flock provides a small cross-process advisory-lock primitive built
// on POSIX flock (Unix) / LockFileEx (Windows). It exists so that checkpoint
// and strategy can both serialize on shared resources without one taking
// the other as an import dependency.
package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquirePollInterval is how often AcquireContext retries a non-blocking
// flock attempt while waiting for a contended lock to free up.
const acquirePollInterval = 10 * time.Millisecond

// Acquire takes an exclusive advisory lock on path, creating the file if
// needed. The returned release closes the file, which drops the flock.
// Callers must invoke release exactly once. The lock file persists between
// runs — flock state is held by the file descriptor, not by the inode on
// disk — so the lockfile contents are immaterial.
//
// Acquire blocks indefinitely on a contended lock and cannot be interrupted
// by context cancellation. Callers that need a bounded wait must use
// AcquireContext instead.
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

// AcquireContext behaves like Acquire, except the wait for a contended lock
// is bounded by ctx: it repeatedly attempts a non-blocking flock
// (LOCK_EX|LOCK_NB) and polls at acquirePollInterval until either the lock
// is obtained or ctx is done, in which case it returns ctx.Err(). Use this
// instead of Acquire whenever the caller has a deadline that must bound
// lock acquisition — the blocking syscall.Flock(LOCK_EX) used by Acquire
// ignores context cancellation entirely.
func AcquireContext(ctx context.Context, path string) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // canonical context cancellation
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}

	ticker := time.NewTicker(acquirePollInterval)
	defer ticker.Stop()
	for {
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // file descriptors are non-negative; standard Go pattern for syscall.Flock
		if flockErr == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(flockErr, syscall.EWOULDBLOCK) && !errors.Is(flockErr, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("flock: %w", flockErr)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err() //nolint:wrapcheck // canonical context cancellation
		case <-ticker.C:
		}
	}
}
