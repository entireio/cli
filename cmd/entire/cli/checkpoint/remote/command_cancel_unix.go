//go:build unix

package remote

import (
	"fmt"
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel SIGKILLs the whole process group on ctx-cancel.
// exec.Cmd's default Cancel only kills `git` itself, leaving any transport-helper
// grandchild alive and holding the output pipe open.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
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
