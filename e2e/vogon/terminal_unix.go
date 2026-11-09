//go:build darwin || linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// holdTerminalLikeTUI turns off canonical input on stdin's terminal for the
// interactive loop, the way a real agent's TUI holds the terminal while it
// runs. A git hook spawned under it then sees the same terminal state it sees
// under Claude Code or Codex, rather than a shell it could prompt on. Echo and
// CR-to-NL translation stay on, so tmux still shows typed prompts and Enter
// still ends a line. The returned func restores the original mode.
func holdTerminalLikeTUI() (restore func()) {
	fd := int(os.Stdin.Fd())
	orig, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		return func() {} // not a terminal: headless input, nothing to hold
	}
	held := *orig
	held.Lflag &^= unix.ICANON
	held.Cc[unix.VMIN] = 1
	held.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, setTermiosReq, &held); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, setTermiosReq, orig) }
}
