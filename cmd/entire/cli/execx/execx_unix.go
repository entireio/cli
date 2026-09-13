//go:build unix

package execx

import (
	"fmt"
	"os/exec"
	"syscall"
)

// detachFromTTY puts the child in a new session with no controlling terminal.
// Any subsequent open of /dev/tty by the child (or its descendants) fails.
func detachFromTTY(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// killProcessGroupOnCancel SIGKILLs the whole process group on ctx-cancel.
// exec.Cmd's default Cancel only kills the direct child, leaving any descendant
// (a sandbox or transport helper) alive and holding the output pipe open.
//
// Setpgid is skipped when the caller already asked for Setsid (NonInteractive):
// setsid(2) makes the child a session leader, and setpgid(2) on a session leader
// fails with EPERM — which surfaces from fork/exec as a bare "operation not
// permitted" that reads like a sandbox problem rather than a misconfiguration.
// Skipping it costs nothing: a session leader is already its own process-group
// leader with pgid == pid, so the negative-PID kill below still reaches the group.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if !cmd.SysProcAttr.Setsid {
		cmd.SysProcAttr.Setpgid = true
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID = whole group (leader pid == pgid). ESRCH = already exited.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("kill process group: %w", err)
		}
		return nil
	}
}
