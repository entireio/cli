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
// stdout/stderr sent to the null device. Best-effort: every error is swallowed — callers
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

	cmd := detachedCommand(executable, dir, args...)
	if err := cmd.Start(); err != nil {
		return
	}
	// Release the process so it can run independently of the parent.
	//nolint:errcheck // best effort — the child continues regardless
	_ = cmd.Process.Release()
}

// detachedCommand builds the child SpawnDetached starts. Separate from the
// spawn so tests can check how the child is wired without forking it.
//
// Stdout and stderr stay nil, which os/exec opens as the null device. A
// non-*os.File writer such as io.Discard would instead hand the child a pipe
// drained by a goroutine in this process; once this process exits, the child's
// next write to that pipe raises SIGPIPE and kills it. A child that reports
// progress on stderr (`__opf_flush` does) would die on its first line.
func detachedCommand(executable, dir string, args ...string) *exec.Cmd {
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
