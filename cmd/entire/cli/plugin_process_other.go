//go:build !linux && !freebsd && !windows

package cli

import (
	"context"
	"os/exec"
)

// configurePluginProcess does nothing here: macOS (and the other BSDs) have no
// kernel mechanism to end a child when its parent dies. Signals this process
// can catch are covered by the cancel path and ForceQuitPlugins; a SIGKILL of
// the CLI still leaves the plugin running, reparented to launchd.
//
// Closing that gap would take a lifeline: an inherited pipe (cmd.ExtraFiles,
// its fd named in an ENTIRE_LIFELINE_FD variable) whose EOF the plugin SDK
// treats as "parent gone". Deliberately not a watchdog process.
func configurePluginProcess(context.Context, *exec.Cmd) {}
