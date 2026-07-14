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
