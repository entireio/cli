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
		b.WriteString(renderPanel("Agents", renderAgentsView(view.AgentCards, view.Mode, styles), width, styles))
	} else {
		b.WriteString(renderPanel("Sessions", renderSessionList(view.Sessions, styles), width, styles))
	}
	b.WriteString("\n\n")

	// Panel 4: Repos · Worktrees · Labels (3-column bottom panel).
	b.WriteString(renderBottomPanel(view, styles))

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

func renderBottomPanel(view View, styles Styles) string {
	// 3 columns: Repos (optional), Worktrees, Labels.
	// Simple left-aligned columns; width split handled by caller's terminal.
	var cols []string

	if len(view.Repos) > 0 {
		var rb strings.Builder
		rb.WriteString(styles.label.Render("Repos") + "\n")
		for _, r := range view.Repos {
			fmt.Fprintf(&rb, "%-20s %d sess\n", r.Repo, r.SessionCount)
		}
		cols = append(cols, strings.TrimRight(rb.String(), "\n"))
	}

	if len(view.Worktrees) > 0 {
		var wb strings.Builder
		wb.WriteString(styles.label.Render("Worktrees") + "\n")
		for _, w := range view.Worktrees {
			marker := " "
			if w.HasUncommitted {
				marker = styles.warn.Render("⇈")
			}
			id := w.WorktreeID
			if id == "" {
				id = "(default)"
			}
			fmt.Fprintf(&wb, "%-20s %d sess %s\n", id, w.SessionCount, marker)
		}
		cols = append(cols, strings.TrimRight(wb.String(), "\n"))
	}

	if len(view.Labels) > 0 {
		var lb strings.Builder
		lb.WriteString(styles.label.Render("Labels") + "\n")
		// Use max count as scale for gradient bar so the topmost label is full.
		maxCount := 0
		for _, l := range view.Labels {
			if l.Count > maxCount {
				maxCount = l.Count
			}
		}
		for _, l := range view.Labels {
			bar := renderGradientBar(l.Count, maxCount, styles)
			fmt.Fprintf(&lb, "%-16s %s\n", l.Label, bar)
		}
		cols = append(cols, strings.TrimRight(lb.String(), "\n"))
	}

	if len(cols) == 0 {
		return ""
	}
	return strings.Join(cols, "\n\n")
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
