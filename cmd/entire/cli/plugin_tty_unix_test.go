//go:build unix

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// terminalSentInterrupt, run in a session whose controlling terminal is a pty
// (as under a shell): a plugin in this process's foreground group got the
// terminal's Ctrl-C; one in a group of its own did not.
func TestTerminalSentInterrupt_UnderATerminal(t *testing.T) {
	t.Parallel()
	const marker = "ENTIRE_TEST_TERMINAL_SENT_INTERRUPT_CHILD"
	if os.Getenv(marker) != "" {
		fmt.Printf("same=%t own=%t\n", terminalSentInterruptFor(t, false), terminalSentInterruptFor(t, true))
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTerminalSentInterrupt_UnderATerminal$")
	cmd.Env = append(os.Environ(), marker+"=1")
	terminal, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: 24, Cols: 100}, &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	out, _ := io.ReadAll(terminal) //nolint:errcheck // ends with EIO when the child exits
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "same=true own=false") {
		t.Errorf("child reported %q, want same=true own=false", out)
	}
}

// terminalSentInterruptFor starts a plugin stand-in, in this process's group
// or (ownGroup) a new one, and asks terminalSentInterrupt about it.
func terminalSentInterruptFor(t *testing.T, ownGroup bool) bool {
	t.Helper()
	plugin := exec.CommandContext(t.Context(), "sleep", "30")
	plugin.SysProcAttr = &syscall.SysProcAttr{Setpgid: ownGroup}
	if err := plugin.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = plugin.Process.Kill() //nolint:errcheck // test cleanup
		_ = plugin.Wait()         //nolint:errcheck // killed above
	}()
	return terminalSentInterrupt(plugin.Process.Pid)
}
