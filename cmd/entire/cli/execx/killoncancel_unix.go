//go:build unix

package execx

import (
	"os/exec"
	"syscall"
	"time"
)

// KillOnCancel configures cmd so that when its context is cancelled the entire
// process group is terminated, and any inherited stdio pipes are forcibly
// closed after waitDelay. This is necessary for subprocesses that fork helpers
// (e.g. `git push` over HTTPS spawns `git-remote-https` and credential
// helpers): SIGKILL on the direct child leaves grandchildren holding the
// inherited pipes, and CombinedOutput stays blocked on Read until they exit on
// their own. Setpgid lets Cancel kill the whole group; WaitDelay is a backstop
// in case a process slips out of the group somehow.
func KillOnCancel(cmd *exec.Cmd, waitDelay time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
}
