//go:build windows

package agents

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setupProcessGroup configures the command for process cleanup on Windows.
// CREATE_NEW_PROCESS_GROUP lets the process be terminated cleanly.
// CREATE_NO_WINDOW starts it the way a desktop app (Claude Code, Codex) starts
// its agent shell: in a console with no window, which the agent's git and hooks
// inherit. That is the Windows counterpart of Setsid on Unix: the hooks face
// what they face on a user's machine (#2725), not the test runner's console.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
	cmd.Cancel = func() error {
		return cmd.Process.Signal(os.Kill)
	}
}
