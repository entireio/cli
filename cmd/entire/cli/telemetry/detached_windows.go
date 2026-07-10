//go:build windows

package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// SpawnDetached starts executable with args as a fully detached background
// process and returns immediately. On Windows it uses
// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS so the subprocess survives the
// parent's exit, roots at the temp dir ("/" does not exist), inherits the
// environment, and discards stdio. It Start + Release so nothing is waited on.
//
// Used for best-effort fire-and-forget work — analytics and the daily pricing
// refresh — that must never block or outlive-block the foreground CLI.
func SpawnDetached(executable string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), executable, args...)

	// Detach from parent console so subprocess survives parent exit.
	// CREATE_NEW_PROCESS_GROUP: own Ctrl+C group (prevents signal propagation).
	// DETACHED_PROCESS: fully detach from parent's console.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}

	// Use temp dir since "/" doesn't exist on Windows.
	cmd.Dir = os.TempDir()

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
