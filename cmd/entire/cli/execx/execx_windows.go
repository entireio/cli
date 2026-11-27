//go:build windows

package execx

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachFromTTY configures cmd to run without an inherited console so any
// /dev/tty-style probe in the child fails. Sets CREATE_NEW_PROCESS_GROUP and
// DETACHED_PROCESS so the child has no console and no signal coupling.
func detachFromTTY(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}

// detachBackgroundWorker configures cmd as a background worker whose own
// subprocesses stay invisible. A DETACHED_PROCESS worker has no console, so
// every console program it starts (git, for one) allocates a fresh console with
// a visible window. CREATE_NO_WINDOW instead gives the worker a console without
// a window, which its children inherit.
//
// The catch: CONIN$ still opens on that console, so a child that prompts hangs
// instead of flashing. Workers are safe today because they run only local git
// commands and Entire's own prompts go through
// interactive.CanPromptInteractively, which reports no window.
//
// Do not combine with NonInteractive on the same cmd: Windows ignores
// CREATE_NO_WINDOW when DETACHED_PROCESS is also set, so the worker would
// silently lose its console again.
func detachBackgroundWorker(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}
