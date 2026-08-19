//go:build !windows

package cli

import (
	"fmt"
	"os/exec"
	"runtime"
)

// openBrowserPlatform launches the platform's URL opener with browserURL as a
// single argv element. No shell is involved.
func openBrowserPlatform(browserURL string) error {
	var command string

	switch runtime.GOOS {
	case darwinGOOS:
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	// exec.Command, not exec.CommandContext: this is deliberately
	// fire-and-forget, and Release below detaches the child so a cancelled
	// context could not kill it anyway (Cmd.Cancel calls Process.Kill, which
	// fails once the process is released). Passing a context would only leave
	// Start's watchCtx goroutine blocked forever on a channel Wait never
	// drains — one leaked goroutine, retaining the Cmd, per browser open.
	cmd := exec.Command(command, browserURL) //nolint:noctx // see above: cancellation is inert here and costs a leaked goroutine
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command %q: %w", command, err)
	}

	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release browser process: %w", err)
	}

	return nil
}
