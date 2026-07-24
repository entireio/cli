package flock

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireContext_Uncontended(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.lock")
	release, err := AcquireContext(context.Background(), path)
	if err != nil {
		t.Fatalf("AcquireContext() error = %v, want nil", err)
	}
	release()
}

func TestAcquireContext_DeadlineExceededWhenContended(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.lock")
	holderRelease, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v, want nil", err)
	}
	defer holderRelease()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = AcquireContext(ctx, path)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("AcquireContext() error = nil, want context deadline exceeded")
	}
	if err != context.DeadlineExceeded { //nolint:errorlint // AcquireContext returns ctx.Err() directly, not wrapped
		t.Fatalf("AcquireContext() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("AcquireContext() took %v, want bounded by ctx deadline", elapsed)
	}
}

func TestAcquireContext_CanceledWhenContended(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.lock")
	holderRelease, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v, want nil", err)
	}
	defer holderRelease()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	_, err = AcquireContext(ctx, path)
	if err != context.Canceled { //nolint:errorlint // AcquireContext returns ctx.Err() directly, not wrapped
		t.Fatalf("AcquireContext() error = %v, want context.Canceled", err)
	}
}

func TestAcquireContext_SucceedsAfterContendedLockReleases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.lock")
	holderRelease, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v, want nil", err)
	}
	time.AfterFunc(30*time.Millisecond, holderRelease)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	release, err := AcquireContext(ctx, path)
	if err != nil {
		t.Fatalf("AcquireContext() error = %v, want nil once contended lock releases", err)
	}
	release()
}

func TestAcquireContext_AlreadyDoneContext(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.lock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := AcquireContext(ctx, path)
	if err != context.Canceled { //nolint:errorlint // AcquireContext returns ctx.Err() directly, not wrapped
		t.Fatalf("AcquireContext() error = %v, want context.Canceled", err)
	}
}
