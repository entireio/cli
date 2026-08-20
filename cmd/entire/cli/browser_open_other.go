//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// browserOpenerCommand names the platform's URL opener, or "" when this
// platform has none we know how to drive.
func browserOpenerCommand() string {
	switch runtime.GOOS {
	case darwinGOOS:
		return "open"
	case "linux":
		return "xdg-open"
	default:
		return ""
	}
}

// browserOpenerAvailable reports whether asking this machine to open a URL
// has a realistic chance of putting it in front of the user. It gates what
// the login prompt advertises: offering "[Enter] Open browser" on a headless
// box only trades a keystroke for an exec error, which is how a login on an
// SSH session ended up printing `exec: "xdg-open": executable file not found
// in $PATH` right after telling the user to press Enter.
//
// The opener binary must exist, and on Linux there must also be a graphical
// session for it to hand the URL to — xdg-open on a display-less server
// either fails or opens a terminal browser the user (sitting somewhere else)
// never sees. WSL is included because its opener bridges to the Windows host
// without a local X/Wayland display.
//
// This is deliberately a check on what to *advertise*, not a veto: pressing
// Enter anyway still attempts the open as long as the binary exists, so an
// unusual-but-working setup is never locked out.
func browserOpenerAvailable() bool {
	command := browserOpenerCommand()
	if command == "" {
		return false
	}
	if _, err := exec.LookPath(command); err != nil {
		return false
	}
	if runtime.GOOS == darwinGOOS {
		return true
	}
	return hasGraphicalSession()
}

// hasGraphicalSession reports whether a display server (or a WSL bridge to
// the Windows host) is reachable from this process's environment.
func hasGraphicalSession() bool {
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "WSL_DISTRO_NAME", "WSL_INTEROP"} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// openBrowserPlatform launches the platform's URL opener with browserURL as a
// single argv element. No shell is involved.
func openBrowserPlatform(browserURL string) error {
	command := browserOpenerCommand()
	if command == "" {
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	// Resolve the opener first so a machine without one gets a sentence a
	// user can act on instead of a nested exec error.
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("no URL opener on this machine (%s is not installed)", command)
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
