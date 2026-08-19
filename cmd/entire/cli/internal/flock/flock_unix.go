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

// pollInterval is how often the bounded AcquireContext path retries a
// non-blocking lock while waiting for a deadline.
const pollInterval = 25 * time.Millisecond

// Acquire takes an exclusive advisory lock on path, creating the file if
// needed. The returned release closes the file, which drops the flock.
// Callers must invoke release exactly once. The lock file persists between
// runs — flock state is held by the file descriptor, not by the inode on
// disk — so the lockfile contents are immaterial.
//
// Acquire blocks indefinitely until the lock is available. Use AcquireContext
// with a deadline to bound the wait.
func Acquire(path string) (release func(), err error) {
	return AcquireContext(context.Background(), path)
}

// AcquireContext behaves like Acquire but honors ctx. When ctx carries a
// deadline it polls a non-blocking lock until the lock is acquired or the
// deadline/cancellation fires, returning a wrapped ctx.Err() on timeout. When
// ctx has no deadline it takes the same blocking kernel path as Acquire, so
// existing callers keep their exact behavior. This lets latency-critical hooks
// (turn-start) bound their wait and degrade gracefully instead of stalling
// behind a long-running lock holder (e.g. checkpoint condensation).
func AcquireContext(ctx context.Context, path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}

	// Fast path: no deadline -> block in the kernel exactly like the historical
	// Acquire. This preserves behavior (and efficiency) for callers that must
	// wait as long as it takes, such as turn-end checkpoint condensation.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // file descriptors are non-negative; standard Go pattern for syscall.Flock
			_ = f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		return func() { _ = f.Close() }, nil
	}

	// Bounded path: poll a non-blocking lock until acquired or ctx is done.
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // see above
		if lockErr == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("flock: %w", lockErr)
		}
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("flock: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
