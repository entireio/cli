package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
)

// openCodeExecutable returns the program to run for an OpenCode command: the
// opencode on PATH, else the one OpenCode's install script put in
// ~/.opencode/bin, else the bare name, so the command fails with its own
// not-found error.
//
// The install script adds ~/.opencode/bin to PATH only in the shell's rc file.
// Entire's hooks, and the git hooks fired by the agent's own commits, run with
// the OpenCode process's environment, which lacks it whenever OpenCode was not
// started from that shell: a desktop app, or a service started by systemd or
// launchd.
//
// A relative home is not used: the path would resolve against the working
// directory, which inside a hook is the repository, and run a file committed
// there.
func openCodeExecutable() string {
	if path, err := exec.LookPath(openCodeBinary); err == nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return openCodeBinary
	}
	// Given a path, LookPath checks only that file: it must exist and be
	// executable (on Windows, carry a PATHEXT extension).
	if path, err := exec.LookPath(filepath.Join(home, ".opencode", "bin", openCodeBinary)); err == nil {
		return path
	}
	return openCodeBinary
}
