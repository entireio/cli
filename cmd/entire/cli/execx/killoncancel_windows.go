//go:build windows

package execx

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// KillOnCancel configures cmd so that when its context is cancelled the
// process and any descendants are terminated and any inherited stdio pipes
// are forcibly closed after waitDelay.
//
// Windows specifics: CREATE_NEW_PROCESS_GROUP isolates the child from the
// parent's console signals, but neither it nor Process.Kill terminates
// descendants — git over HTTPS spawns git-remote-https plus credential
// helpers that survive a direct kill and keep the inherited stdio open. We
// shell out to taskkill.exe /F /T (force + tree) so the whole descendant
// chain dies. If taskkill is unavailable, we fall back to the direct kill
// and rely on waitDelay to bound the user-visible hang.
func KillOnCancel(cmd *exec.Cmd, waitDelay time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill.exe", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		taskkillErr := kill.Run()
		if taskkillErr == nil {
			return nil
		}
		// taskkill is missing or refused — fall back to direct kill so at
		// least the visible child terminates. WaitDelay still closes the
		// pipes within bounded time, so the caller does not hang.
		if killErr := cmd.Process.Kill(); killErr != nil {
			return fmt.Errorf("taskkill: %w; fallback kill: %w", taskkillErr, killErr)
		}
		return taskkillErr
	}
	cmd.WaitDelay = waitDelay
}
