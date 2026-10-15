package execx

import (
	"context"
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
	if testing.Testing() {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		return
	}

	cmd := newDetachedCmd(executable, dir, args...)
	if err := cmd.Start(); err != nil {
		return
	}
	// Release the process so it can run independently of the parent.
	//nolint:errcheck // best effort — the child continues regardless
	_ = cmd.Process.Release()
}

// newDetachedCmd builds the detached child's command. Stdout and Stderr stay
// nil so os/exec opens the null device FOR THE CHILD instead of creating
// pipes: a non-*os.File writer (io.Discard was used here before) makes
// os/exec allocate an OS pipe whose read end lives in THIS process, so once a
// short-lived parent — a git hook — exits, the child's next stdout/stderr
// write takes EPIPE, and the Go runtime kills a Go child on SIGPIPE to fds
// 1/2, terminating it mid-work. Process.Release does not prevent that; only
// the absence of parent-held pipes does.
func newDetachedCmd(executable, dir string, args ...string) *exec.Cmd {
	// context.Background(): the child must outlive the parent, so it is never
	// tied to a cancellable context.
	cmd := exec.CommandContext(context.Background(), executable, args...)
	detachFromTTY(cmd)
	cmd.Dir = dir
	if cmd.Dir == "" {
		cmd.Dir = os.TempDir()
	}
	cmd.Env = os.Environ()
	return cmd
}
