//go:build darwin || linux

package interactive

import (
	"os"

	"golang.org/x/sys/unix"
)

// ttyInRawMode reports whether the terminal behind f has canonical (line) input
// disabled — i.e. a full-screen program owns the screen rather than a shell we
// can prompt.
//
// Terminal-UI git clients (lazygit, gitui, tig, …) run git as a child process
// while keeping the controlling terminal in raw mode for their own screen and
// key handling. Such a child — including a git hook, and therefore `entire` —
// still inherits that controlling terminal, so it can open /dev/tty
// successfully. The /dev/tty probe alone therefore cannot tell a TUI apart from
// a shell, and prompting there is broken in both directions: our output paints
// over the TUI's screen, and our line read races the TUI's key reader for the
// user's keystrokes, so the answer may never arrive and the git command appears
// to hang (the reported symptom was lazygit freezing on the "Link this commit
// to session context?" prompt).
//
// Canonical input mode (ICANON) is the signal that separates the two: a shell
// restores the terminal to canonical mode before running a foreground command,
// while a TUI holds it in raw mode for as long as it owns the screen. That is
// also exactly the distinction the TUIs themselves draw — lazygit restores
// cooked mode when it deliberately hands the terminal to a child (editor,
// interactive rebase, custom subprocess commands), which is when prompting does
// work and should happen.
//
// Checking the terminal mode rather than the hook's file descriptors is
// deliberate: git's hook stdio plumbing varies by git version (stdin is
// /dev/null, and stdout/stderr may be inherited or captured), so an isatty
// check on those descriptors is not a stable signal, while terminal ownership
// is.
//
// rawModeIoctl is the platform's termios read ioctl (rawmode_darwin.go,
// rawmode_linux.go); rawmode_other.go covers platforms without termios.
func ttyInRawMode(f *os.File) bool {
	termios, err := unix.IoctlGetTermios(int(f.Fd()), rawModeIoctl) //nolint:gosec // G115: uintptr->int is safe for fd
	if err != nil {
		// Can't tell — fail open so an unexpected ioctl failure never silently
		// disables prompting. This check may only ever suppress prompts we
		// positively know are unusable.
		return false
	}
	return termios.Lflag&unix.ICANON == 0
}
