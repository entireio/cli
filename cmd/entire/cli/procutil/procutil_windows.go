//go:build windows

package procutil

import (
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// killProcessGroupOnCancel terminates the process tree on ctx-cancel, so
// grandchildren that inherited the output pipe die too.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return execx.KillProcessTree(cmd.Process.Pid)
	}
}
