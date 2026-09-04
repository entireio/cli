//go:build unix

package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireRevalidatesReplacedLockFileInode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "resource.lock")
	testRevalidatesReplacedLockFileInode(t,
		func(time.Duration) (func(), error) {
			return Acquire(path)
		},
		func() error {
			return os.Remove(path)
		},
	)
}

func TestAcquireContextRevalidatesReplacedLockFileInode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "resource.lock")
	testRevalidatesReplacedLockFileInode(t,
		func(timeout time.Duration) (func(), error) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			release, err := AcquireContext(ctx, path)
			if err != nil {
				cancel()
				return nil, err
			}
			return func() {
				release()
				cancel()
			}, nil
		},
		func() error {
			return os.Remove(path)
		},
	)
}

func TestAcquireInRevalidatesReplacedLockFileInode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir) //nolint:noinlineerr // test fixture: the root's base is the temp dir itself
	if err != nil {
		t.Fatalf("OpenRoot() error = %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	name := "resource.lock"
	testRevalidatesReplacedLockFileInode(t,
		func(time.Duration) (func(), error) {
			return AcquireIn(root, name)
		},
		func() error {
			return os.Remove(filepath.Join(dir, name))
		},
	)
}

func TestAcquireContextInRevalidatesReplacedLockFileInode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir) //nolint:noinlineerr // test fixture: the root's base is the temp dir itself
	if err != nil {
		t.Fatalf("OpenRoot() error = %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	name := "resource.lock"
	testRevalidatesReplacedLockFileInode(t,
		func(timeout time.Duration) (func(), error) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			release, err := AcquireContextIn(ctx, root, name)
			if err != nil {
				cancel()
				return nil, err
			}
			return func() {
				release()
				cancel()
			}, nil
		},
		func() error {
			return os.Remove(filepath.Join(dir, name))
		},
	)
}

func TestLockFileStaleInodeRetryIsBounded(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "resource.lock")
	_, err := lockFile(context.Background(), openPathLockFile(t, path), func(*os.File) (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("lockFile() error = nil, want stale inode retry failure")
	}
}

func TestLockFileStaleInodeRetryHonorsContextDeadline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "resource.lock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	_, err := lockFile(ctx, openPathLockFile(t, path), func(*os.File) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockFile() error = %v, want context deadline exceeded", err)
	}
}

func openPathLockFile(t *testing.T, path string) func() (*os.File, error) {
	t.Helper()

	return func() (*os.File, error) {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	}
}

func testRevalidatesReplacedLockFileInode(t *testing.T, acquire func(time.Duration) (func(), error), removeLockFile func() error) {
	t.Helper()

	releaseOld, err := acquire(2 * time.Second)
	if err != nil {
		t.Fatalf("Acquire old lock: %v", err)
	}

	waiter := make(chan func(), 1)
	errs := make(chan error, 1)
	go func() {
		release, err := acquire(2 * time.Second)
		if err != nil {
			errs <- err
			return
		}
		waiter <- release
	}()

	time.Sleep(50 * time.Millisecond)
	if err := removeLockFile(); err != nil {
		t.Fatalf("remove old lock path: %v", err)
	}

	releaseNew, err := acquire(2 * time.Second)
	if err != nil {
		t.Fatalf("Acquire new lock: %v", err)
	}

	releaseOld()

	select {
	case release := <-waiter:
		release()
		t.Fatal("waiter acquired the unlinked inode while the current lock file was held")
	case err := <-errs:
		t.Fatalf("waiter failed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	releaseNew()

	select {
	case release := <-waiter:
		release()
	case err := <-errs:
		t.Fatalf("waiter failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not acquire after current lock was released")
	}
}
