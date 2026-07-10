//go:build unix

package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// SpawnDetached starts executable with args as a fully detached background
// process and returns immediately. On Unix it gets its own process group
// (Setpgid) so it survives the parent's exit, roots at dir (or "/" when dir is
// empty or missing) so it holds a defined working directory, inherits the
// environment, and discards stdio. It Start + Release so nothing is ever waited
// on.
//
// dir sets the child's working directory. Analytics passes "" to hold no
// specific directory (rooting at "/" as before); the daily pricing refresh
// passes the project directory so the worker resolves project-level settings
// relative to it. A dir that no longer exists falls back to "/".
//
// Used for best-effort fire-and-forget work — analytics and the daily pricing
// refresh — that must never block or outlive-block the foreground CLI.
func SpawnDetached(dir, executable string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), executable, args...)

	// Detach from parent process group so subprocess survives parent exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Root at the requested directory, or "/" when unset/missing.
	cmd.Dir = resolveDetachedDir(dir, "/")

	// Inherit environment (may be needed for network config).
	cmd.Env = os.Environ()

	// Discard stdout/stderr to prevent output leaking to parent's terminal.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	// Start the process (non-blocking).
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached process: %w", err)
	}

	// Release the process so it can run independently.
	//nolint:errcheck // Best effort - process should continue regardless
	_ = cmd.Process.Release()
	return nil
}
