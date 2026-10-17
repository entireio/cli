//go:build unix

package execx

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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

// killProcessGroupOnCancel SIGKILLs the whole process group on ctx-cancel.
// exec.Cmd's default Cancel only kills the direct child, leaving any descendant
// (a sandbox or transport helper) alive and holding the output pipe open.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID = whole group (leader pid == pgid). ESRCH = already exited.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("kill process group: %w", err)
		}
		return nil
	}
}

// markInheritedFDsCloseOnExec sets close-on-exec on every descriptor above
// stderr. os/exec passes a child only its stdio and ExtraFiles, but it does not
// close descriptors this process inherited without close-on-exec: those pass on
// to every child. A git hook inherits git's pipes that way — the write end of a
// remote helper's stdin among them — and a detached child holding one keeps
// the helper from seeing EOF, so the user's `git push` cannot finish until the
// child exits. Descriptors Go opened itself are already close-on-exec.
//
// It runs in the hook process, so later children of the hook lose those
// descriptors too; the one legitimate use, a user's GIT_TRACE=<fd>, makes git
// warn and carry on.
func markInheritedFDsCloseOnExec() {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No fd directory (no /proc in a container, BSD without fdescfs):
		// mark every possible descriptor instead. CloseOnExec on a closed
		// one is a harmless EBADF.
		var lim syscall.Rlimit
		maxFD := uint64(4096)
		if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil && lim.Cur < maxFD {
			maxFD = lim.Cur
		}
		for fd := 3; uint64(fd) < maxFD; fd++ {
			syscall.CloseOnExec(fd)
		}
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= 2 {
			continue
		}
		syscall.CloseOnExec(fd)
	}
}
