//go:build unix

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// spawnDetachedWingmanReview spawns a detached subprocess to run the wingman review.
// On Unix, this uses process group detachment so the subprocess continues
// after the parent exits.
func spawnDetachedWingmanReview(payloadJSON string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}

	//nolint:gosec // G204: payloadJSON is controlled internally, not user input
	cmd := exec.CommandContext(context.Background(), executable, "wingman", "__review", payloadJSON)

	// Detach from parent process group so subprocess survives parent exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Don't hold the working directory
	cmd.Dir = "/"

	// Inherit environment (needed for PATH, git config, etc.)
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
