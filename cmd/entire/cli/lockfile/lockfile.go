// Package lockfile provides cross-process file locks. The OS releases
// the lock on process exit, so there's no stale-lock recovery problem.
package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrLocked is returned by Acquire when another process holds the lock.
var ErrLocked = errors.New("lockfile already held")

// Lock represents an acquired exclusive lock. The OS releases on
// process exit if Release is not called.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive OS-level lock on path. Returns ErrLocked
// if another process holds the lock; other errors indicate I/O or
// permission failures. The PID written into the file is advisory
// diagnostic only — see ReadHolderPID. FD_CLOEXEC is set so
// subprocesses don't inherit the lock FD.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: lock path is supplied by the caller (trusted)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := tryLockExclusive(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := writePID(f); err != nil {
		_ = unlock(f) //nolint:errcheck // best-effort cleanup; the writePID failure is what propagates
		_ = f.Close()
		return nil, fmt.Errorf("write PID to lock file %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// WithTimeout acquires path, runs fn while holding the lock, and releases it.
// If another process holds the lock, it retries until ctx is canceled or timeout
// elapses. The returned lock error wraps ErrLocked on timeout.
func WithTimeout(ctx context.Context, path string, timeout time.Duration, fn func() error) (err error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	var lk *Lock
	for {
		lk, err = Acquire(path)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrLocked) {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire lock %s: %w", path, ctx.Err())
		case <-deadline.C:
			if holder := ReadHolderPID(path); holder > 0 {
				return fmt.Errorf("lock %s held by PID %d: %w", path, holder, ErrLocked)
			}
			return fmt.Errorf("lock %s held: %w", path, ErrLocked)
		case <-ticker.C:
		}
	}
	defer func() {
		if releaseErr := lk.Release(); err == nil && releaseErr != nil {
			err = releaseErr
		}
	}()

	return fn()
}

// Release releases the OS lock and closes the file. Idempotent.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	unlockErr := unlock(f)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file: %w", closeErr)
	}
	return nil
}

// ReadHolderPID returns the PID written into the lock file, or 0 if the
// file is empty/unreadable/contains garbage. Best-effort; for diagnostic
// messages only.
func ReadHolderPID(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // G304: lock path is supplied by the caller (trusted)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func writePID(f *os.File) error {
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("write PID: %w", err)
	}
	return nil
}
