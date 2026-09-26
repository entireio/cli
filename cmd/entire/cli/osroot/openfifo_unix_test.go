//go:build unix

package osroot_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// TestOpenNoFollow_RefusesFifoWithoutBlocking is the case the refusal exists
// for. open(2) on a FIFO with no writer blocks until one arrives, and none of
// these helpers passes O_NONBLOCK, so before the Lstat gate this call hung the
// process instead of failing it — `entire doctor` in a repo with a FIFO at
// .claude/settings.json was unkillable short of SIGINT.
//
// The test is written as a race against a timer rather than a plain error
// assertion, because a regression here does not fail: it hangs, and an
// unguarded assertion would take the whole package's timeout with it.
//
// Unix-only by build constraint rather than a runtime skip: syscall.Mkfifo does
// not exist on Windows, and a runtime guard still has to compile.
func TestOpenNoFollow_RefusesFifoWithoutBlocking(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "settings.json"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	done := make(chan error, 1)
	go func() {
		_, openErr := osroot.OpenNoFollow(root, "settings.json")
		done <- openErr
	}()

	select {
	case openErr := <-done:
		if !errors.Is(openErr, osroot.ErrNotRegularFile) {
			t.Errorf("OpenNoFollow(fifo) error = %v, want ErrNotRegularFile", openErr)
		}
	case <-time.After(10 * time.Second):
		// Deliberately not t.Fatal: the goroutine is still parked in openat and
		// will stay there, so say what happened and let the process exit.
		t.Error("OpenNoFollow(fifo) blocked instead of returning; the pre-open type check is gone")
	}
}
