package recap

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	placeholderDash = "—"
	unknownAgent    = "unknown"
)

// RenderStatic produces the full static (non-TUI) recap output for a view.
// Width is the outer terminal width — the function clamps to a 60-cell
// minimum and uses the space for panel borders + content padding.
//
// Accessible / piped mode: pass styles from NewStyles(false). Output
// then contains no ANSI escapes and no Unicode borders; panels degrade to
// section headers separated by blank lines.
func RenderStatic(view View, styles Styles, width int) string {
	if width < 60 {
		width = 60
	}
	var b strings.Builder

	// Panel 1: Summary band.
	b.WriteString(renderPanel(view.Title, renderSummaryBand(view.Summary, styles), width, styles))
	b.WriteString("\n\n")

	// Panel 2: Activity strip — range-dependent.
	b.WriteString(renderActivityStrip(view, styles))
	b.WriteString("\n\n")

	// Panel 3: Agents (default) or Sessions (fallback when view has no AgentCards).
	if len(view.AgentCards) > 0 {
		innerWidth := width - 4 // 2 border chars + 1 padding char each side
		b.WriteString(renderPanel("Agents", renderAgentsView(view.AgentCards, view.Mode, innerWidth, styles), width, styles))
	} else {
		b.WriteString(renderPanel("Sessions", renderSessionList(view.Sessions, styles), width, styles))
	}

	return b.String()
}

func renderSessionList(rows []SessionRow, styles Styles) string {
	if len(rows) == 0 {
		return styles.muted.Render("(no sessions in range)")
	}
	var b strings.Builder
	for i, r := range rows {
		agent := r.Agent
		if agent == "" {
			agent = unknownAgent
		}
		label := r.Label
		if label == "" {
			label = styles.muted.Render(placeholderDash)
		}
		hint := string(r.Hint)
		hintStyle := styles.styleForHint(r.Hint)
		hintRendered := ""
		if hint != "" {
			hintRendered = hintStyle.Render("▶ " + hint)
		}
		line := fmt.Sprintf("%s %-14s %-8s ▪ %-16s %2d cp   %s",
			styles.accent.Render(r.Badge),
			agent,
			r.Span,
			label,
			r.Checkpoints,
			hintRendered,
		)
		b.WriteString(strings.TrimRight(line, " "))
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// helpers -------------------------------------------------------------------

// formatTokens renders a token count compactly: 142000 → "142k", 1500000 → "1.5M".
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return strconv.Itoa(n)
	}
}
