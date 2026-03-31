//go:build windows

package agents

import (
	"os"
	"os/exec"
	"syscall"
)

// setupProcessGroup configures the command for process cleanup on Windows.
// Uses CREATE_NEW_PROCESS_GROUP so the process can be terminated cleanly.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Cancel = func() error {
		return cmd.Process.Signal(os.Kill)
	}
}
