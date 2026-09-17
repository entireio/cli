//go:build !windows

package uiform

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/creack/pty"
)

// clearedFromFirstRow matches a cursor-up over the rendered form followed by
// an erase to the end of the display — CUU then ED0, the pair that removes an
// answered prompt. The row count is left open on purpose; see the assertion.
var clearedFromFirstRow = regexp.MustCompile(`\x1b\[[0-9]+A\x1b\[J`)

// An empty Form.View is not enough: the renderer must move back over the
// question before erasing it. Exercise the actual terminal output, because
// accessible-mode tests bypass the renderer that left answered prompts behind.
func TestConfirmationClearsPromptAfterAnswer(t *testing.T) {
	t.Parallel()
	terminal, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close() })
	t.Cleanup(func() { _ = input.Close() })
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const question = "Install the entire-graph plugin?"
	output := make(chan string, 1)
	go func() {
		var transcript strings.Builder
		answered := false
		buf := make([]byte, 4096)
		for {
			n, readErr := terminal.Read(buf)
			transcript.Write(buf[:n])
			if !answered && strings.Contains(transcript.String(), question) {
				answered = true
				if _, writeErr := io.WriteString(terminal, "\r"); writeErr != nil {
					cancel()
				}
			}
			if readErr != nil {
				output <- transcript.String()
				return
			}
		}
	}()
	answer := true
	form := New(huh.NewGroup(huh.NewConfirm().Title(question).Value(&answer))).
		WithProgramOptions(tea.WithEnvironment([]string{"TERM=xterm-256color"})).
		WithAccessible(false).WithInput(input).WithOutput(input)
	err = form.RunWithContext(ctx)
	_ = input.Close() // End the reader after the final render has been flushed.
	if err != nil {
		t.Fatal(err)
	}
	transcript := <-output
	// The form ends with the cursor on its help row, so erasing from there
	// alone leaves the question and choices visible: the renderer has to move
	// back up over the form first, then erase to the end of the display.
	//
	// The number of rows is deliberately not pinned. It is a property of the
	// form's height, which a field, theme or terminal-width change moves, and
	// a literal "\x1b[4A\x1b[J" fails such a change with a message about
	// prompt clearing — which is not what broke.
	if !clearedFromFirstRow.MatchString(transcript) {
		t.Fatalf("completed prompt was not erased from its first row: %q", transcript)
	}
	if !answer {
		t.Fatal("Enter did not retain the default Yes answer")
	}
}
