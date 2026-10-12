//go:build windows

package execx

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// detachFromTTY configures cmd to run without an inherited console so any
// /dev/tty-style probe in the child fails. Sets CREATE_NEW_PROCESS_GROUP and
// DETACHED_PROCESS so the child has no console and no signal coupling.
func detachFromTTY(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}

// killProcessGroupOnCancel uses the system taskkill to terminate the process
// tree. If tree termination fails, still kill the direct child; WaitDelay
// bounds inherited pipes. Resolve the system binary without consulting PATH.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if systemDir, err := windows.GetSystemDirectory(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			kill := exec.CommandContext(ctx, filepath.Join(systemDir, "taskkill.exe"), "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
			kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if err := kill.Run(); err == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
}
