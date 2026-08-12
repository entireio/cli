//go:build darwin || linux

package interactive

import (
	"testing"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// TestTTYInRawMode_PTY exercises the real signal on a real terminal: a freshly
// opened pty is in canonical mode (the shape a shell leaves the terminal in
// while a foreground `git commit` runs), and putting it in raw mode (the shape a
// TUI git client like lazygit holds it in) must flip detection.
func TestTTYInRawMode_PTY(t *testing.T) {
	t.Parallel()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("cannot open a pty here: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	if ttyInRawMode(tty) {
		t.Error("ttyInRawMode(fresh pty) = true; want false (canonical mode)")
	}

	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if !ttyInRawMode(tty) {
		t.Error("ttyInRawMode(raw pty) = false; want true (a TUI owns the terminal)")
	}

	if err := term.Restore(int(tty.Fd()), state); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if ttyInRawMode(tty) {
		t.Error("ttyInRawMode(restored pty) = true; want false (back to canonical mode)")
	}
}
