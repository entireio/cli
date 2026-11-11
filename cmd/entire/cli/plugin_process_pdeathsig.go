//go:build linux || freebsd

package cli

import (
	"context"
	"os/exec"
	"syscall"
)

// configurePluginProcess has the kernel SIGKILL the plugin when this process
// dies, however it dies — including SIGKILL, which no handler of ours sees.
// runTrackedPlugin keeps Start..Wait on one OS thread, because Pdeathsig
// fires when the starting thread exits rather than the process.
//
// Only the plugin itself is covered, not processes it starts; those are the
// plugin's to manage.
func configurePluginProcess(_ context.Context, cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
