//go:build unix

package telemetry

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// spawnDetachedAnalytics spawns a detached subprocess to send analytics.
// On Unix, this uses process group detachment so the subprocess continues
// after the parent exits.
func spawnDetachedAnalytics(payloadJSON string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}

	//nolint:gosec // G204: payloadJSON is controlled internally, not user input
	cmd := exec.CommandContext(context.Background(), executable, "__send_analytics", payloadJSON)

	// Detach from parent process group so subprocess survives parent exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Don't hold the working directory
	cmd.Dir = "/"

	// Inherit environment (may be needed for network config)
	cmd.Env = os.Environ()

	// Discard stdout/stderr to prevent output leaking to parent's terminal
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	// Start the process (non-blocking)
	if err := cmd.Start(); err != nil {
		return
	}

	// Release the process so it can run independently
	//nolint:errcheck // Best effort - process should continue regardless
	_ = cmd.Process.Release()
}
