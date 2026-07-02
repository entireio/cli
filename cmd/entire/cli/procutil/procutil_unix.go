//go:build unix

package procutil

import (
	"fmt"
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel SIGKILLs the whole process group on ctx-cancel, so
// grandchildren that inherited the output pipe die too.
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
