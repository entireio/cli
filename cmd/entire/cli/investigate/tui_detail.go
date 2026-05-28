package investigate

import (
	"fmt"
	"strings"
)

// entryLine renders one timelineEntry as a single display line truncated
// to maxWidth cells.
func entryLine(e timelineEntry, maxWidth int) string {
	var raw string
	switch e.kind {
	case "started":
		raw = fmt.Sprintf("[turn %d started %s]", e.turn, e.when.Format("15:04:05"))
	case "finished":
		parts := []string{fmt.Sprintf("[turn %d finished %s]", e.turn, formatDuration(e.duration))}
		if e.stance != "" {
			parts = append(parts, e.stance)
		}
		if e.findings != "" {
			parts = append(parts, fmt.Sprintf("%q", e.findings))
		}
		raw = strings.Join(parts, " · ")
	case "failed":
		parts := []string{fmt.Sprintf("[turn %d failed %s]", e.turn, formatDuration(e.duration))}
		if e.errStr != "" {
			parts = append(parts, e.errStr)
		}
		raw = strings.Join(parts, " · ")
	default:
		raw = fmt.Sprintf("[turn %d %s]", e.turn, e.kind)
	}
	return truncateDisplayWidth(sanitizeDisplayText(raw), maxWidth)
}

// detailView renders the alt-screen drill-in for one agent. The output
// is padded to exactly termHeight lines. termHeight and termWidth come
// from WindowSizeMsg via investigateTUIModel, so the rendered frame
// always fills the visible terminal.
func detailView(row agentRow, scroll, termWidth, termHeight int) string {
	if termWidth < 1 {
		termWidth = 80
	}
	if termHeight < 3 {
		termHeight = 3
	}
	bodyHeight := termHeight - 2

	headerContent := fmt.Sprintf("─── %s (%d turns) ", sanitizeDisplayText(row.name), len(row.buffer))
	header := padDisplayWidthWith(headerContent, termWidth, "─")

	lines := buildDetailBody(row.buffer, scroll, bodyHeight, termWidth)
	for len(lines) < bodyHeight {
		lines = append(lines, strings.Repeat(" ", termWidth))
	}

	footerText := "←/→ switch agent · ↑/↓ scroll · Esc back · Ctrl+C cancel"
	footer := padDisplayWidth(footerText, termWidth)

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(footer)
	return b.String()
}

// buildDetailBody returns the visible body lines, clamped to bodyHeight.
// scroll is the index of the LAST visible entry; the body shows
// [scroll-bodyHeight+1 ... scroll] inclusive.
func buildDetailBody(buffer []timelineEntry, scroll, bodyHeight, termWidth int) []string {
	if len(buffer) == 0 || bodyHeight <= 0 {
		return nil
	}
	if scroll < 0 {
		scroll = 0
	}
	if scroll >= len(buffer) {
		scroll = len(buffer) - 1
	}
	end := scroll + 1
	start := end - bodyHeight
	if start < 0 {
		start = 0
	}
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		lines = append(lines, entryLine(buffer[i], termWidth))
	}
	return lines
}
