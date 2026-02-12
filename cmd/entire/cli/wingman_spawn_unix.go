//go:build unix

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// spawnDetachedWingmanReview spawns a detached subprocess to run the wingman review.
// repoRoot is the repository root used to locate the log file.
// payloadPath is the path to the JSON payload file.
// On Unix, this uses process group detachment so the subprocess continues
// after the parent exits.
func spawnDetachedWingmanReview(repoRoot, payloadPath string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}

	//nolint:gosec // G204: payloadPath is controlled internally, not user input
	cmd := exec.CommandContext(context.Background(), executable, "wingman", "__review", payloadPath)

	// Detach from parent process group so subprocess survives parent exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Don't hold the working directory
	cmd.Dir = "/"

	// Inherit environment (needed for PATH, git config, etc.)
	cmd.Env = os.Environ()

	// Redirect stderr to a log file for debugging the background process.
	// This catches panics, errors, and all wingmanLog() output.
	cmd.Stdout = io.Discard
	var logFile *os.File
	logDir := filepath.Join(repoRoot, ".entire", "logs")
	if mkErr := os.MkdirAll(logDir, 0o750); mkErr == nil {
		//nolint:gosec // G304: path is constructed from repoRoot + constants
		if f, openErr := os.OpenFile(filepath.Join(logDir, "wingman.log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); openErr == nil {
			logFile = f
			cmd.Stderr = f
		} else {
			cmd.Stderr = io.Discard
		}
	} else {
		cmd.Stderr = io.Discard
	}

	// Start the process (non-blocking)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[wingman] Failed to spawn review subprocess: %v\n", err)
		if logFile != nil {
			_ = logFile.Close()
		}
		return
	}

	pid := cmd.Process.Pid
	fmt.Fprintf(os.Stderr, "[wingman] Review subprocess spawned (pid=%d)\n", pid)

	// The log file handle is inherited by the child process and will be
	// closed when that process exits. Don't close it here.

	// Release the process so it can run independently
	//nolint:errcheck // Best effort - process should continue regardless
	_ = cmd.Process.Release()
}

// spawnDetachedWingmanApply spawns a detached subprocess to auto-apply the
// pending REVIEW.md via claude --continue. Called from the stop hook when a
// review is pending after the agent turn ends.
func spawnDetachedWingmanApply(repoRoot string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}

	//nolint:gosec // G204: repoRoot is from paths.RepoRoot(), not user input
	cmd := exec.CommandContext(context.Background(), executable, "wingman", "__apply", repoRoot)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	cmd.Dir = "/"
	cmd.Env = os.Environ()

	cmd.Stdout = io.Discard
	var applyLogFile *os.File
	logDir := filepath.Join(repoRoot, ".entire", "logs")
	if mkErr := os.MkdirAll(logDir, 0o750); mkErr == nil {
		//nolint:gosec // G304: path is constructed from repoRoot + constants
		if f, openErr := os.OpenFile(filepath.Join(logDir, "wingman.log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); openErr == nil {
			applyLogFile = f
			cmd.Stderr = f
		} else {
			cmd.Stderr = io.Discard
		}
	} else {
		cmd.Stderr = io.Discard
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[wingman] Failed to spawn apply subprocess: %v\n", err)
		if applyLogFile != nil {
			_ = applyLogFile.Close()
		}
		return
	}

	pid := cmd.Process.Pid
	fmt.Fprintf(os.Stderr, "[wingman] Apply subprocess spawned (pid=%d)\n", pid)

	// The log file handle is inherited by the child process and will be
	// closed when that process exits. Don't close it here.

	//nolint:errcheck // Best effort - process should continue regardless
	_ = cmd.Process.Release()
}
