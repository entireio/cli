//go:build unix

package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestIndexRecordsNoEntries_RefusesAFifoWithoutBlocking pins the reason the
// reader stats before it opens. Opening a FIFO with no writer blocks until one
// arrives, and this code has no deadline of its own, runs inside a commit hook,
// and exists for the filesystem class that hangs — so a named pipe where the
// index should be has to be refused, not opened.
func TestIndexRecordsNoEntries_RefusesAFifoWithoutBlocking(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "index")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	type result struct {
		empty bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		empty, err := IndexRecordsNoEntries(path)
		done <- result{empty, err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, ErrNotAnIndexFile) {
			t.Fatalf("want ErrNotAnIndexFile for a FIFO, got %v", got.err)
		}
		if got.empty {
			t.Fatal("a FIFO was reported as an empty index")
		}
	case <-time.After(5 * time.Second):
		// The goroutine is stuck in open(2) and cannot be reclaimed; a real
		// hook process would be stuck the same way, which is the bug.
		t.Fatal("IndexRecordsNoEntries blocked on a FIFO instead of refusing it")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the FIFO should be untouched: %v", err)
	}
}
