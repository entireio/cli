//go:build windows

package jsonutil

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIsRenameContention(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errorAccessDenied,
		errorSharingViolation,
		&os.LinkError{Op: "rename", Old: "staged", New: "session.json", Err: errorSharingViolation},
	} {
		if !isRenameContention(err) {
			t.Errorf("isRenameContention(%v) = false, want true", err)
		}
	}
	if isRenameContention(os.ErrNotExist) {
		t.Error("isRenameContention(ErrNotExist) = true, want false")
	}
}

func TestRenameWithRetry_WindowsContentionThenSuccess(t *testing.T) {
	t.Parallel()
	ops := defaultAtomicWriteOps()
	attempts := 0
	ops.rename = func(string, string) error {
		attempts++
		if attempts < 3 {
			return errorSharingViolation
		}
		return nil
	}
	waits := 0
	ops.wait = func(_ context.Context, delay time.Duration) error {
		waits++
		if delay != renameBackoff {
			t.Errorf("retry delay = %s, want %s", delay, renameBackoff)
		}
		return nil
	}

	if err := renameWithRetry(context.Background(), "staged", "session.json", ops); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("rename attempts = %d, want 3", attempts)
	}
	if waits != 2 {
		t.Fatalf("rename waits = %d, want 2", waits)
	}
}

func TestRenameWithRetry_WindowsContentionExhausted(t *testing.T) {
	t.Parallel()
	ops := defaultAtomicWriteOps()
	attempts := 0
	ops.rename = func(string, string) error {
		attempts++
		return errorAccessDenied
	}
	waits := 0
	ops.wait = func(_ context.Context, delay time.Duration) error {
		waits++
		if delay != renameBackoff {
			t.Errorf("retry delay = %s, want %s", delay, renameBackoff)
		}
		return nil
	}

	err := renameWithRetry(context.Background(), "staged", "session.json", ops)
	if !errors.Is(err, errorAccessDenied) {
		t.Fatalf("error = %v, want access denied cause", err)
	}
	if !strings.Contains(err.Error(), "another process may be reading session.json") {
		t.Fatalf("error = %q, want reader contention hint", err)
	}
	if attempts != renameAttempts {
		t.Fatalf("rename attempts = %d, want %d", attempts, renameAttempts)
	}
	if waits != renameAttempts-1 {
		t.Fatalf("rename waits = %d, want %d", waits, renameAttempts-1)
	}
}

func TestRenameWithRetry_WindowsContentionHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ops := defaultAtomicWriteOps()
	attempts := 0
	ops.rename = func(string, string) error {
		attempts++
		return errorSharingViolation
	}

	err := renameWithRetry(ctx, "staged", "session.json", ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if attempts != 0 {
		t.Fatalf("rename attempts = %d, want 0", attempts)
	}
}
