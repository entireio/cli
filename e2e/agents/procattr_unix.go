//go:build unix

package agents

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup starts the command in a new session, which is also a new
// process group, so the entire tree can be killed on cancellation. A new
// session also has no controlling terminal: a headless agent run from a
// developer's shell then faces the hooks the way it does in CI and when an IDE
// or desktop app launches it, instead of offering them the developer's terminal
// to prompt on.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
