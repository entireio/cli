//go:build unix

package execx

import (
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
