//go:build windows

package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// acquirePollInterval is how often AcquireContext retries a non-blocking
// lock attempt while waiting for a contended lock to free up.
const acquirePollInterval = 10 * time.Millisecond

// Acquire takes an exclusive lock on path via Windows LockFileEx. The
// returned release unlocks and closes the file. Callers must invoke release
// exactly once.
//
// Acquire blocks indefinitely on a contended lock and cannot be interrupted
// by context cancellation. Callers that need a bounded wait must use
// AcquireContext instead.
func Acquire(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock flock: %w", err)
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		_ = f.Close()
	}, nil
}

// AcquireContext behaves like Acquire, except the wait for a contended lock
// is bounded by ctx: it repeatedly attempts a non-blocking LockFileEx
// (LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY) and polls at
// acquirePollInterval until either the lock is obtained or ctx is done, in
// which case it returns ctx.Err(). Use this instead of Acquire whenever the
// caller has a deadline that must bound lock acquisition.
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
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	for {
		overlapped := new(windows.Overlapped)
		lockErr := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, overlapped)
		if lockErr == nil {
			return func() {
				_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION) && !errors.Is(lockErr, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", lockErr)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err() //nolint:wrapcheck // canonical context cancellation
		case <-ticker.C:
		}
	}
}
