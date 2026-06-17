// Package review — see env.go for package-level rationale.
//
// tui_detail.go provides the alt-screen drill-in renderer. The body content is
// produced by [eventLines] (one or more wrapped lines per event) and fed into
// a bubbles/v2/viewport on [reviewTUIModel]; this file's [detailFrame] is the
// pure-function chrome (header + body + footer) that wraps the viewport's
// pre-rendered output and pads to exactly termHeight lines so every frame has
// the same line count (avoids ghost rows in Bubble Tea's alt-screen diff).
package review

import (
	"fmt"
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// eventLines converts a single Event to one or more display lines for the
// detail view, wrapped to maxWidth display cells. AssistantText preserves
// embedded '\n' as paragraph breaks; other event types render as a single
// sanitized line that wraps only on width overflow.
func eventLines(ev reviewtypes.Event, maxWidth int) []string {
	if maxWidth <= 0 {
		return nil
	}
	var raw string
	switch e := ev.(type) {
	case reviewtypes.Started:
		raw = "[started]"
	case reviewtypes.AssistantText:
		// AssistantText is the only event that can contain meaningful
		// multi-line content; wrapDisplayWidth honors embedded newlines.
		return wrapDisplayWidth(e.Text, maxWidth)
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

	return wrapDisplayWidth(raw, maxWidth)
}

// buildEventLines returns every wrapped body line for the supplied event
// buffer, in order. The result is suitable for feeding into a viewport via
// SetContentLines.
func buildEventLines(buffer []reviewtypes.Event, maxWidth int) []string {
	if len(buffer) == 0 || maxWidth <= 0 {
		return nil
	}
	out := make([]string, 0, len(buffer))
	for _, ev := range buffer {
		out = append(out, eventLines(ev, maxWidth)...)
	}
	return out
}

// detailFrame renders the alt-screen drill-in chrome around a body string. The
// body is the viewport's already-rendered view (already clipped to bodyHeight
// lines by the viewport itself). detailFrame adds:
//
//  1. Header line: "─── <name> (<n> events) ─────────────" (filled to termWidth)
//  2. Body: trimmed/padded to exactly bodyHeight lines, each padded to termWidth.
//  3. Footer line: "←/→ switch agent · Esc back · scroll: PgUp/PgDn/↑/↓/Home/End"
//
// CRITICAL: the rendered string is exactly termHeight lines. Bubble Tea's
// alt-screen diff leaves ghost rows if the line count varies between frames.
func detailFrame(row agentRow, body string, termWidth, termHeight int) string {
	if termWidth < 1 {
		termWidth = 80
	}
	if termHeight < 3 {
		termHeight = 3
	}

	bodyHeight := termHeight - 2
	if bodyHeight < 0 {
		bodyHeight = 0
	}

	headerContent := fmt.Sprintf("─── %s (%d events) ", sanitizeDisplayText(row.name), len(row.buffer))
	header := padDisplayWidthWith(headerContent, termWidth, "─")

	// Normalize the viewport body to exactly bodyHeight lines, each padded to
	// termWidth so frame width is stable.
	bodyLines := splitBodyToHeight(body, bodyHeight, termWidth)

	footerText := "←/→ switch agent · Esc back · scroll: PgUp/PgDn/↑/↓/Home/End"
	footer := padDisplayWidth(footerText, termWidth)

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, line := range bodyLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(footer)
	return b.String()
}

// splitBodyToHeight normalizes a multi-line body string to exactly bodyHeight
// lines, each truncated and padded to termWidth. Missing lines are padded
// with spaces. The bodyHeight cap is a defensive guard: viewport.View()
// should already clip to its Height(), so the overflow-truncation path is
// not expected to trigger in normal use.
func splitBodyToHeight(body string, bodyHeight, termWidth int) []string {
	if bodyHeight <= 0 {
		return nil
	}
	var raw []string
	if body != "" {
		raw = strings.Split(strings.TrimRight(body, "\n"), "\n")
	}
	lines := make([]string, 0, bodyHeight)
	for i := range bodyHeight {
		if i < len(raw) {
			lines = append(lines, padDisplayWidth(raw[i], termWidth))
		} else {
			lines = append(lines, strings.Repeat(" ", termWidth))
		}
	}
	return lines
}
