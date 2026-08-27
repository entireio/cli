package execx

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
)

// SpawnDetached re-execs the current executable as a detached, fire-and-forget
// child running args, surviving the parent's exit (new session on Unix,
// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS on Windows, via detachFromTTY).
// The child runs in dir (os.TempDir() when empty, so the child never holds the
// parent's working directory), inherits the parent's environment, and has its
// stdout/stderr discarded. Best-effort: every error is swallowed — callers
// treat the spawn as advisory background work.
//
// In-process `go test` runs are a no-op: the current executable is the test
// binary, and re-execing it would fork the whole suite. Tests exercise the
// call sites through their spawn seams instead.
func SpawnDetached(dir string, args ...string) {
	//nolint:errcheck // the swallow-everything variant, by contract
	_ = SpawnDetachedErr(dir, args...)
}

// SpawnDetachedErr is SpawnDetached for callers that left something behind for
// the child to pick up (a job file, a claim) and must clean it up when there is
// no child. It reports why the spawn never happened; it says nothing about what
// the child then did, which nobody waits for either way.
//
// The `go test` no-op returns nil: from the caller's side the spawn did not
// fail, it did not happen at all, and reporting a failure would make tests
// exercise a cleanup path production never takes.
func SpawnDetachedErr(dir string, args ...string) error {
	if testing.Testing() {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	// context.Background(): the child must outlive the parent, so it is never
	// tied to a cancellable context.
	cmd := exec.CommandContext(context.Background(), executable, args...)
	detachFromTTY(cmd)
	cmd.Dir = dir
	if cmd.Dir == "" {
		cmd.Dir = os.TempDir()
	}
	cmd.Env = os.Environ()
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached %s: %w", executable, err)
	}
	// Release the process so it can run independently of the parent.
	//nolint:errcheck // best effort — the child continues regardless
	_ = cmd.Process.Release()
	return nil
}
