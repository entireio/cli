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

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// pollInterval is how often the bounded AcquireContext path retries a
// non-blocking lock while waiting for a deadline.
const pollInterval = 25 * time.Millisecond

// Acquire takes an exclusive advisory lock on path, creating the file if
// needed. The returned release closes the file, which drops the flock.
// Callers must invoke release exactly once. The lock file persists between
// runs - flock state is held by the file descriptor, not by the inode on
// disk - so the lockfile contents are immaterial.
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
	return lockFile(ctx, func() (*os.File, error) {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
		if err != nil {
			return nil, err //nolint:wrapcheck // lockFile wraps opener errors once
		}
		return f, nil
	}, func(f *os.File) (bool, error) {
		return holdsCurrentPathInode(f, path)
	})
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
	return lockFile(ctx, func() (*os.File, error) {
		return openLockFileIn(root, name)
	}, func(f *os.File) (bool, error) {
		return holdsCurrentRootInode(f, root, name)
	})
}

// tryLockFile takes a single non-blocking LOCK_EX and never waits. EWOULDBLOCK
// means another open file description holds the lock (ok=false, err=nil); any
// other flock error is a real failure. On success the returned release closes
// the file, which drops the lock.
func tryLockFile(f *os.File) (release func(), ok bool, err error) {
	lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // file descriptors are non-negative; standard Go pattern for syscall.Flock
	if lockErr == nil {
		return func() { _ = f.Close() }, true, nil
	}
	_ = f.Close()
	if errors.Is(lockErr, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("flock: %w", lockErr)
}

// lockFile holds the locking logic shared by the path- and root-based entry
// points, which differ only in how the file is opened and revalidated.
func lockFile(ctx context.Context, open func() (*os.File, error), holdsCurrent func(*os.File) (bool, error)) (release func(), err error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		for range 3 {
			f, err := open()
			if err != nil {
				return nil, fmt.Errorf("open flock: %w", err)
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // file descriptors are non-negative; standard Go pattern for syscall.Flock
				_ = f.Close()
				return nil, fmt.Errorf("flock: %w", err)
			}
			current, err := holdsCurrent(f)
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			if current {
				return func() { _ = f.Close() }, nil
			}
			_ = f.Close()
		}
		return nil, errors.New("flock path changed while acquiring lock")
	}

	staleInodeRetries := 0
	for {
		f, err := open()
		if err != nil {
			return nil, fmt.Errorf("open flock: %w", err)
		}
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // see above
		if lockErr == nil {
			current, err := holdsCurrent(f)
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			if current {
				return func() { _ = f.Close() }, nil
			}
			_ = f.Close()
			staleInodeRetries++
			if staleInodeRetries >= 3 {
				return nil, errors.New("flock path changed while acquiring lock")
			}
		} else {
			_ = f.Close()
			if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("flock: %w", lockErr)
			}
		}
		if err := waitForRetry(ctx); err != nil {
			return nil, err
		}
	}
}

func waitForRetry(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("flock: %w", ctx.Err())
	case <-time.After(pollInterval):
		return nil
	}
}

func holdsCurrentPathInode(f *os.File, path string) (bool, error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat flock path: %w", err)
	}
	return sameInode(f, pathInfo, "path")
}

func holdsCurrentRootInode(f *os.File, root *os.Root, name string) (bool, error) {
	pathInfo, err := osroot.LstatNoSymlinks(root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat flock root path: %w", err)
	}
	return sameInode(f, pathInfo, "root path")
}

func sameInode(f *os.File, pathInfo os.FileInfo, label string) (bool, error) {
	heldInfo, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat flock fd: %w", err)
	}
	held, ok := heldInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat flock fd: unexpected stat type %T", heldInfo.Sys())
	}
	current, ok := pathInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat flock %s: unexpected stat type %T", label, pathInfo.Sys())
	}
	return held.Dev == current.Dev && held.Ino == current.Ino, nil
}
