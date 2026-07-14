package review

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// countLines counts the number of lines (\n-separated) in s.
// A trailing \n does not produce an extra empty line for counting purposes.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	// detailFrame ends without a trailing newline after footer; lines are
	// separated by \n. Count \n occurrences + 1 (for the final segment).
	return strings.Count(s, "\n") + 1
}

// renderDetailViaModel builds a reviewTUIModel populated with the supplied
// events, sized to termWidth × termHeight, and returns the rendered detail
// frame. The viewport is the source of truth for body lines, so we exercise
// rendering through the model rather than calling detailFrame in isolation.
func renderDetailViaModel(t *testing.T, name string, buffer []reviewtypes.Event, termWidth, termHeight int) string {
	t.Helper()
	m := newReviewTUIModel([]string{name}, nil)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
	m = mustModel(t, updated)
	for _, ev := range buffer {
		updated, _ := m.Update(agentEventMsg{agent: name, ev: ev})
		m = mustModel(t, updated)
	}
	// Enter detail mode so refreshDetailContent runs.
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)
	return m.View().Content
}

func TestDetailFrame_PadsToTermHeight(t *testing.T) {
	t.Parallel()
	for _, termHeight := range []int{5, 10, 20, 24} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			out := renderDetailViaModel(t, "agent-a",
				[]reviewtypes.Event{
					reviewtypes.AssistantText{Text: "line1"},
					reviewtypes.AssistantText{Text: "line2"},
				}, 80, termHeight)
			got := countLines(out)
			if got != termHeight {
				t.Errorf("termHeight=%d: expected %d lines, got %d\noutput:\n%s",
					termHeight, termHeight, got, out)
			}
		})
	}
}

func TestDetailFrame_EmptyBuffer_PadsToTermHeight(t *testing.T) {
	t.Parallel()
	termHeight := 10
	out := renderDetailViaModel(t, "agent-a", nil, 80, termHeight)
	got := countLines(out)
	if got != termHeight {
		t.Errorf("empty buffer: expected %d lines, got %d", termHeight, got)
	}
}

func TestDetailFrame_HeaderContainsAgentNameAndCount(t *testing.T) {
	t.Parallel()
	out := renderDetailViaModel(t, "claude-code",
		[]reviewtypes.Event{
			reviewtypes.AssistantText{Text: "a"},
			reviewtypes.AssistantText{Text: "b"},
			reviewtypes.AssistantText{Text: "c"},
		}, 80, 10)
	firstLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(firstLine, "claude-code") {
		t.Errorf("header missing agent name: %q", firstLine)
	}
	if !strings.Contains(firstLine, "3 events") {
		t.Errorf("header missing event count: %q", firstLine)
	}
	if !strings.HasSuffix(firstLine, "─") {
		t.Errorf("header should fill remaining width with rule characters: %q", firstLine)
	}
}

func TestDetailFrame_FooterPresent(t *testing.T) {
	t.Parallel()
	out := renderDetailViaModel(t, "agent-a",
		[]reviewtypes.Event{reviewtypes.AssistantText{Text: "x"}}, 80, 8)
	lines := strings.Split(out, "\n")
	lastLine := lines[len(lines)-1]
	if !strings.Contains(lastLine, "Esc back") {
		t.Errorf("footer missing expected text: %q", lastLine)
	}
}

func TestDetailFrame_LinesFitTerminalWidth(t *testing.T) {
	t.Parallel()
	buffer := []reviewtypes.Event{
		reviewtypes.AssistantText{Text: strings.Repeat("界", 20)},
		reviewtypes.ToolCall{Name: "wide", Args: strings.Repeat("🚀", 20)},
		reviewtypes.RunError{Err: errors.New(strings.Repeat("日", 20))},
	}

	for _, width := range []int{10, 20, 40, 80} {
		t.Run(fmt.Sprintf("width %d", width), func(t *testing.T) {
			t.Parallel()
			out := renderDetailViaModel(t, "claude-code-with-a-very-wide-name", buffer, width, 12)
			for i, line := range strings.Split(out, "\n") {
				if got := ansi.StringWidth(line); got > width {
					t.Fatalf("line %d width = %d, want <= %d:\n%q", i, got, width, line)
				}
			}
		})
	}
}

func TestDetailFrame_MultibyteRuneSafe(t *testing.T) {
	t.Parallel()
	multibyte := strings.Repeat("日", 20) // 20 runes, 60 bytes
	termWidth := 10
	out := renderDetailViaModel(t, "agent-a",
		[]reviewtypes.Event{reviewtypes.AssistantText{Text: multibyte}}, termWidth, 8)
	for i, line := range strings.Split(out, "\n") {
		runes := utf8.RuneCountInString(line)
		if runes > termWidth {
			t.Errorf("line %d has %d runes (>%d): %q", i, runes, termWidth, line)
		}
	}
}

func TestDetailFrame_ANSIStripped(t *testing.T) {
	t.Parallel()
	// Include CSI sequences that codex emits (cursor-hide / cursor-show).
	ansiText := "hello\x1b[?25lworld\x1b[?25h"
	out := renderDetailViaModel(t, "agent-a",
		[]reviewtypes.Event{reviewtypes.AssistantText{Text: ansiText}}, 80, 6)
	if strings.Contains(out, "\x1b") {
		t.Error("output should have ANSI sequences stripped")
	}
	if !strings.Contains(out, "helloworld") {
		t.Errorf("expected helloworld after ANSI strip; got %q", out)
	}
}

func TestDetailFrame_EventTypes_Rendered(t *testing.T) {
	t.Parallel()
	buffer := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.ToolCall{Name: "read_file", Args: "foo.go"},
		reviewtypes.Tokens{In: 100, Out: 50},
		reviewtypes.Finished{Success: true},
		reviewtypes.RunError{Err: errors.New("oops")},
	}
	out := renderDetailViaModel(t, "agent-a", buffer, 120, 10)
	checks := []string{"[started]", "[tool: read_file]", "in=100", "[finished: success]", "[error: oops]"}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got:\n%s", want, out)
		}
	}
}

// TestDetailFrame_WrapsLongAssistantText is load-bearing: a long AssistantText
// must wrap across multiple visible body lines rather than being truncated.
// This is the whole point of switching the drill-in body to a viewport.
func TestDetailFrame_WrapsLongAssistantText(t *testing.T) {
	t.Parallel()
	// 200+ character AssistantText (space-separated tokens so word wrap can fire).
	long := strings.TrimSpace(strings.Repeat("word ", 50)) // 50 words × 5 chars - trailing space ≈ 249 chars
	if utf8.RuneCountInString(long) < 200 {
		t.Fatalf("test setup: expected >= 200 runes, got %d", utf8.RuneCountInString(long))
	}
	termWidth := 40
	termHeight := 20 // ample body height
	out := renderDetailViaModel(t, "agent-a",
		[]reviewtypes.Event{reviewtypes.AssistantText{Text: long}}, termWidth, termHeight)

	// Every line fits within termWidth.
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if got := ansi.StringWidth(line); got > termWidth {
			t.Fatalf("line %d width = %d, want <= %d:\n%q", i, got, termWidth, line)
		}
	}

	// The body must contain more than one line of "word" content. A single
	// truncated line would only show one fragment; wrapping should produce
	// several body lines beyond the header.
	wordLineCount := 0
	for _, line := range lines {
		if strings.Contains(line, "word") {
			wordLineCount++
		}
	}
	if wordLineCount < 4 {
		t.Errorf("expected long AssistantText to wrap onto multiple visible lines (got %d body lines with 'word'); output:\n%s",
			wordLineCount, out)
	}
}

// TestDetailFrame_PreservesAssistantTextNewlines pins that embedded newlines in
// AssistantText act as paragraph breaks rather than being collapsed to spaces.
// Multi-paragraph review findings must remain readable after the wrap helper
// turns them into multiple body lines.
func TestDetailFrame_PreservesAssistantTextNewlines(t *testing.T) {
	t.Parallel()
	text := "first paragraph here\nsecond paragraph here\nthird paragraph here"
	out := renderDetailViaModel(t, "agent-a",
		[]reviewtypes.Event{reviewtypes.AssistantText{Text: text}}, 80, 12)
	for _, want := range []string{"first paragraph here", "second paragraph here", "third paragraph here"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected paragraph %q to survive in detail output; got:\n%s", want, out)
		}
	}
}
