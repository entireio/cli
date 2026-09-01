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
	path := filepath.Join(t.TempDir(), "resource.lock")

	releaseOld, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire old lock: %v", err)
	}

	waiter := make(chan func(), 1)
	errs := make(chan error, 1)
	go func() {
		release, err := Acquire(path)
		if err != nil {
			errs <- err
			return
		}
		waiter <- release
	}()

	time.Sleep(50 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove old lock path: %v", err)
	}

	releaseNew, err := Acquire(path)
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

func TestAcquireContextRevalidatesReplacedLockFileInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.lock")

	releaseOld, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire old lock: %v", err)
	}

	waiter := make(chan func(), 1)
	errs := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		release, err := AcquireContext(ctx, path)
		if err != nil {
			errs <- err
			return
		}
		waiter <- release
	}()

	time.Sleep(50 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove old lock path: %v", err)
	}

	releaseNew, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire new lock: %v", err)
	}
	defer releaseNew()

	releaseOld()

	select {
	case release := <-waiter:
		release()
		t.Fatal("waiter acquired the unlinked inode while the current lock file was held")
	case err := <-errs:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter error = %v, want context deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not time out while current lock was held")
	}
}
