//go:build unix

package strategy

import (
	"fmt"
	"os/exec"
	"syscall"
)

// killEntityDeltasProcessGroupOnCancel SIGKILLs the whole process group when the
// producer's deadline expires. Same pattern (and same reason) as
// checkpoint/remote's killProcessGroupOnCancel: exec.Cmd's default Cancel kills
// only the process it started, leaving any worker the producer forked alive and
// still holding the output pipe open past the timeout.
func killEntityDeltasProcessGroupOnCancel(cmd *exec.Cmd) {
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
			return fmt.Errorf("kill entity deltas process group: %w", err)
		}
		return nil
	}
}
