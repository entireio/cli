//go:build !windows

package strategy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// After a timeout the prompt must not leave a read behind on the terminal:
// pre-push goes on to the upload, and a leftover read would take the next line
// typed, such as an answer to a git credential prompt. The child reads through
// /dev/tty, as the hook does: on macOS it takes no read deadline, unlike the
// pty's own device path.
func TestReadPromptAnswer_TimeoutLeavesTheNextLineForTheNextReader(t *testing.T) {
	t.Parallel()
	const marker = "ENTIRE_TEST_PROMPT_TTY_CHILD"
	if os.Getenv(marker) == "1" {
		tty, err := interactive.OpenPromptTTY()
		if err != nil {
			t.Fatalf("open prompt terminal: %v", err)
		}
		defer tty.Close()
		if _, err := readPromptAnswer(tty, 50*time.Millisecond); !errors.Is(err, errPromptTimedOut) {
			t.Fatalf("err = %v, want errPromptTimedOut", err)
		}
		fmt.Println("TIMED_OUT")
		line, err := readPromptAnswer(tty, 5*time.Second)
		fmt.Printf("NEXT %q %v\n", line, err)
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadPromptAnswer_TimeoutLeavesTheNextLineForTheNextReader$")
	cmd.Env = append(os.Environ(), marker+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := pty.StartWithAttrs(cmd, nil, &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0})
	if err != nil {
		t.Skipf("start child on a pty: %v", err)
	}
	defer terminal.Close()
	go func() { _, _ = io.Copy(io.Discard, terminal) }() //nolint:errcheck // drains echo so the child never blocks writing

	var next string
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		switch line := lines.Text(); {
		case line == "TIMED_OUT":
			if _, err := io.WriteString(terminal, "hunter2\n"); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(line, "NEXT "):
			next = line
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	if want := `NEXT "hunter2\n" <nil>`; next != want {
		t.Fatalf("the next reader got %s, want %s; a read left behind by the timeout took it", next, want)
	}
}
