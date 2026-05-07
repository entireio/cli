package review

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// countLines counts the number of lines (\n-separated) in s.
// A trailing \n does not produce an extra empty line for counting purposes.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	// detailView ends without a trailing newline after footer; lines are
	// separated by \n.  Count \n occurrences + 1 (for the final segment).
	return strings.Count(s, "\n") + 1
}

func makeBuffer(texts ...string) []reviewtypes.Event {
	buf := make([]reviewtypes.Event, len(texts))
	for i, t := range texts {
		buf[i] = reviewtypes.AssistantText{Text: t}
	}
	return buf
}

func TestDetailView_PadsToTermHeight(t *testing.T) {
	t.Parallel()
	for _, termHeight := range []int{5, 10, 20, 24} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			row := agentRow{
				name:   "agent-a",
				buffer: makeBuffer("line1", "line2"),
			}
			out := detailView(row, 0, 80, termHeight)
			got := countLines(out)
			if got != termHeight {
				t.Errorf("termHeight=%d: expected %d lines, got %d\noutput:\n%s",
					termHeight, termHeight, got, out)
			}
		})
	}
}

func TestDetailView_EmptyBuffer_PadsToTermHeight(t *testing.T) {
	t.Parallel()
	row := agentRow{name: "agent-a", buffer: nil}
	termHeight := 10
	out := detailView(row, 0, 80, termHeight)
	got := countLines(out)
	if got != termHeight {
		t.Errorf("empty buffer: expected %d lines, got %d", termHeight, got)
	}
}

func TestDetailView_HeaderContainsAgentNameAndCount(t *testing.T) {
	t.Parallel()
	row := agentRow{
		name:   "claude-code",
		buffer: makeBuffer("a", "b", "c"),
	}
	out := detailView(row, 0, 80, 10)
	firstLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(firstLine, "claude-code") {
		t.Errorf("header missing agent name: %q", firstLine)
	}
	if !strings.Contains(firstLine, "3 events") {
		t.Errorf("header missing event count: %q", firstLine)
	}
}

func TestDetailView_FooterPresent(t *testing.T) {
	t.Parallel()
	row := agentRow{name: "agent-a", buffer: makeBuffer("x")}
	out := detailView(row, 0, 80, 8)
	lines := strings.Split(out, "\n")
	lastLine := lines[len(lines)-1]
	if !strings.Contains(lastLine, "Esc back") {
		t.Errorf("footer missing expected text: %q", lastLine)
	}
}

func TestDetailView_LineTruncation_RuneSafe(t *testing.T) {
	t.Parallel()
	// Use a multi-byte UTF-8 string: each '日' is 3 bytes but 1 rune.
	multibyte := strings.Repeat("日", 20) // 20 runes, 60 bytes
	row := agentRow{
		name:   "agent-a",
		buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: multibyte}},
	}
	termWidth := 10
	out := detailView(row, 0, termWidth, 5)
	// Each body line must not exceed termWidth runes.
	for i, line := range strings.Split(out, "\n") {
		runes := utf8.RuneCountInString(line)
		if runes > termWidth {
			t.Errorf("line %d has %d runes (>%d): %q", i, runes, termWidth, line)
		}
	}
}

func TestDetailView_ANSIStripped(t *testing.T) {
	t.Parallel()
	// Include CSI sequences that codex emits (cursor-hide / cursor-show).
	ansiText := "hello\x1b[?25lworld\x1b[?25h"
	row := agentRow{
		name:   "agent-a",
		buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: ansiText}},
	}
	out := detailView(row, 0, 80, 5)
	if strings.Contains(out, "\x1b") {
		t.Error("output should have ANSI sequences stripped")
	}
	if !strings.Contains(out, "helloworld") {
		t.Errorf("expected helloworld after ANSI strip; got %q", out)
	}
}

func TestDetailView_Scrolling_LeadingLinesHidden(t *testing.T) {
	t.Parallel()
	// 5 events; termHeight=6 (1 header + 3 body + 1 footer = 5; we set 6 to leave 4 body lines).
	texts := []string{"line0", "line1", "line2", "line3", "line4"}
	row := agentRow{name: "agent-a", buffer: makeBuffer(texts...)}

	// scroll=4 (max): shows events 1-4 in body (if bodyHeight=4).
	termHeight := 6
	out := detailView(row, 4, 80, termHeight)
	if strings.Contains(out, "line0") {
		t.Error("line0 should not appear when scrolled to the bottom with 4 body lines")
	}
	if !strings.Contains(out, "line4") {
		t.Errorf("line4 should appear at scroll=4; output:\n%s", out)
	}

	// scroll=0: shows first bodyHeight events.
	out0 := detailView(row, 0, 80, termHeight)
	if !strings.Contains(out0, "line0") {
		t.Errorf("line0 should appear at scroll=0; output:\n%s", out0)
	}
}

func TestDetailView_EventTypes_Rendered(t *testing.T) {
	t.Parallel()
	row := agentRow{
		name: "agent-a",
		buffer: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.ToolCall{Name: "read_file", Args: "foo.go"},
			reviewtypes.Tokens{In: 100, Out: 50},
			reviewtypes.Finished{Success: true},
			reviewtypes.RunError{Err: errors.New("oops")},
		},
	}
	out := detailView(row, 4, 120, 10)
	checks := []string{"[started]", "[tool: read_file]", "in=100", "[finished: success]", "[error: oops]"}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got:\n%s", want, out)
		}
	}
}
