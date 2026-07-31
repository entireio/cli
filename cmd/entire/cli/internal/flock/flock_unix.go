//go:build unix

package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

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
	return onceRelease(func() { _ = f.Close() }), nil
}

// AcquireContext behaves like Acquire, except the wait for a contended lock
// is bounded by ctx: it repeatedly attempts a non-blocking flock
// (LOCK_EX|LOCK_NB) and polls at acquirePollInterval until either the lock
// is obtained or ctx is done, in which case it returns ctx.Err(). Use this
// instead of Acquire whenever the caller has a deadline that must bound
// lock acquisition — the blocking syscall.Flock(LOCK_EX) used by Acquire
// ignores context cancellation entirely.
//
// When ctx has no Done channel (Background/TODO) it can neither time out nor
// be canceled, so AcquireContext falls back to the blocking Acquire rather
// than spinning at acquirePollInterval forever. Cancelable contexts keep the
// poll loop so cancellation is still observed.
func AcquireContext(ctx context.Context, path string) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // canonical context cancellation
	}
	// A context with no Done channel (Background/TODO) can neither time out nor
	// be canceled, so the poll loop would spin forever at acquirePollInterval.
	// Block on the OS lock instead — the kernel wakes us when it frees, no spin.
	// Cancelable contexts (WithCancel) keep the poll loop so cancellation works.
	if ctx.Done() == nil {
		return Acquire(path)
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
			return onceRelease(func() { _ = f.Close() }), nil
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
