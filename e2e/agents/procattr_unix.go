//go:build unix

package agents

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup configures the command to run in its own process group
// so the entire tree can be killed on cancellation.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
