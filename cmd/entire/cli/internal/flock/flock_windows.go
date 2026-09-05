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

// pollInterval is how often the bounded AcquireContext path retries a
// non-blocking lock while waiting for a deadline.
const pollInterval = 25 * time.Millisecond

// Acquire takes an exclusive lock on path via Windows LockFileEx. The
// returned release unlocks and closes the file. Callers must invoke release
// exactly once. Acquire blocks indefinitely until the lock is available; use
// AcquireContext with a deadline to bound the wait.
func Acquire(path string) (release func(), err error) {
	return AcquireContext(context.Background(), path)
}

// AcquireContext behaves like Acquire but honors ctx. When ctx carries a
// deadline it polls a fail-immediately lock until acquired or the deadline
// fires; otherwise it blocks like Acquire. See the unix implementation for the
// rationale.
func AcquireContext(ctx context.Context, path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}
	return lockFile(ctx, f)
}

// AcquireIn is Acquire for a lock file named inside root. It is the form the
// .git-resident locks use: their names carry agent-supplied session IDs, so
// resolving them through the git common dir's root keeps a name that escaped
// validation from naming a file outside the clone.
func AcquireIn(root *os.Root, name string) (release func(), err error) {
	return AcquireContextIn(context.Background(), root, name)
}

// AcquireContextIn is AcquireContext for a lock file named inside root.
func AcquireContextIn(ctx context.Context, root *os.Root, name string) (release func(), err error) {
	f, err := openLockFileIn(root, name)
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}
	return lockFile(ctx, f)
}

// tryLockFile takes a single fail-immediately exclusive lock and never waits. A
// held lock is reported as ERROR_LOCK_VIOLATION / ERROR_IO_PENDING (ok=false,
// err=nil), mirroring the unix EWOULDBLOCK case; any other error is a real
// failure. On success the returned release unlocks and closes the file.
func tryLockFile(f *os.File) (release func(), ok bool, err error) {
	overlapped := new(windows.Overlapped)
	lockErr := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if lockErr == nil {
		return func() {
			_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
			_ = f.Close()
		}, true, nil
	}
	_ = f.Close()
	if errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION) || errors.Is(lockErr, windows.ERROR_IO_PENDING) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("lock flock: %w", lockErr)
}

// lockFile holds the locking logic shared by the path- and root-based entry
// points, which differ only in how the file was opened.
func lockFile(ctx context.Context, f *os.File) (release func(), err error) {
	overlapped := new(windows.Overlapped)
	releaseFn := func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		_ = f.Close()
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", err)
		}
		return releaseFn, nil
	}

	for {
		lockErr := windows.LockFileEx(windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if lockErr == nil {
			return releaseFn, nil
		}
		// Only lock contention is retryable. LOCKFILE_FAIL_IMMEDIATELY reports a
		// held lock as ERROR_LOCK_VIOLATION (or ERROR_IO_PENDING); any other error
		// is a genuine failure (I/O, bad handle) that must fail fast rather than
		// polling until the deadline and masking the real cause as a timeout —
		// mirroring the unix path, which only retries on EWOULDBLOCK.
		if !errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION) && !errors.Is(lockErr, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", lockErr)
		}
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
