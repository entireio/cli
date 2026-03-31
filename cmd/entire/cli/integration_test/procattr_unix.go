//go:build integration && unix

package integration

import (
	"os/exec"
	"syscall"
)

// detachFromTerminal configures the command to run in a new session,
// detaching from the controlling terminal so interactive prompts don't hang tests.
func detachFromTerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
