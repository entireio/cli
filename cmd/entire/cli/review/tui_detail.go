// Package review — see env.go for package-level rationale.
//
// tui_detail.go provides detailView, the pure-function renderer for the
// alt-screen drill-in view. It renders one agent's live event buffer with
// header/footer chrome and pads to exactly termHeight lines so every frame
// has the same line count (avoids ghost rows in the Bubble Tea alt-screen
// frame diff).
package review

import (
	"fmt"
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// eventLine converts a single Event to a single display line for the detail
// view. The line is stripped of control sequences and truncated to maxWidth
// display cells.
func eventLine(ev reviewtypes.Event, maxWidth int) string {
	var raw string
	switch e := ev.(type) {
	case reviewtypes.Started:
		raw = "[started]"
	case reviewtypes.AssistantText:
		raw = e.Text
	case reviewtypes.ToolCall:
		raw = fmt.Sprintf("[tool: %s] %s", e.Name, e.Args)
	case reviewtypes.Tokens:
		raw = fmt.Sprintf("[tokens in=%d out=%d]", e.In, e.Out)
	case reviewtypes.Finished:
		if e.Success {
			raw = "[finished: success]"
		} else {
			raw = "[finished: failed]"
		}
	case reviewtypes.RunError:
		if e.Err != nil {
			raw = fmt.Sprintf("[error: %v]", e.Err)
		} else {
			raw = "[error]"
		}
	default:
		raw = "[unknown event]"
	}

	return truncateDisplayWidth(sanitizeDisplayText(raw), maxWidth)
}

// detailView renders the alt-screen drill-in for one agent. row is the agentRow
// being inspected. termWidth/termHeight come from WindowSizeMsg.
//
// Rendering:
//  1. Header line: "─── <name> (<n> events) ─────────────" (filled to termWidth)
//  2. Body: events from row.buffer scrolled to detailScroll, one line each,
//     sanitized and truncated to termWidth display cells.
//  3. Footer line: "←/→ switch agent · Esc back · ↑/↓ scroll"
//
// CRITICAL: the rendered string is padded to exactly termHeight lines so every
// frame has the same line count. Bubble Tea's alt-screen frame diff leaves ghost
// rows if the line count varies between frames.
func detailView(row agentRow, detailScroll, termWidth, termHeight int) string {
	if termWidth < 1 {
		termWidth = 80
	}
	if termHeight < 3 {
		termHeight = 3
	}

	// Reserve 1 line for header, 1 for footer; the body fills the rest.
	bodyHeight := termHeight - 2
	if bodyHeight < 0 {
		bodyHeight = 0
	}

	// 1. Header line.
	headerContent := fmt.Sprintf("─── %s (%d events) ", sanitizeDisplayText(row.name), len(row.buffer))
	header := padDisplayWidthWith(headerContent, termWidth, "─")

	// 2. Body lines.
	lines := buildBodyLines(row.buffer, detailScroll, bodyHeight, termWidth)

	// Pad body to exactly bodyHeight lines.
	for len(lines) < bodyHeight {
		lines = append(lines, strings.Repeat(" ", termWidth))
	}

	// 3. Footer line.
	footerText := "←/→ switch agent · Esc back · ↑/↓ scroll"
	footer := padDisplayWidth(footerText, termWidth)

	// Assemble: header + body + footer = termHeight lines total.
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(footer)
	// No trailing newline after footer — the caller (View) adds its own.

	return b.String()
}

// buildBodyLines computes the visible body lines for the detail view.
// It takes the full event buffer, a scroll offset, the maximum number of lines
// to show, and the column width. Returns at most bodyHeight lines.
func buildBodyLines(buffer []reviewtypes.Event, scroll, bodyHeight, termWidth int) []string {
	if len(buffer) == 0 || bodyHeight <= 0 {
		return nil
	}

	// Clamp scroll to valid range.
	if scroll < 0 {
		scroll = 0
	}
	if scroll >= len(buffer) {
		scroll = len(buffer) - 1
	}

	// Determine window: scroll is the index of the LAST visible line so the
	// user sees the most-recent events when auto-scrolling. Work backwards.
	end := scroll + 1 // exclusive upper bound
	start := end - bodyHeight
	if start < 0 {
		start = 0
	}
	// Clamp end to buffer length.
	if end > len(buffer) {
		end = len(buffer)
		start = end - bodyHeight
		if start < 0 {
			start = 0
		}
	}

	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		lines = append(lines, eventLine(buffer[i], termWidth))
	}
	return lines
}
