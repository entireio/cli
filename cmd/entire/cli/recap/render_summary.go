package recap

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderSummaryBand draws the headline summary panel content (no border — the
// caller wraps it in renderPanel). Layout:
//
//	<RangeLabel>
//
//	you   <sessions> sessions  <checkpoints> checkpoints  <tokens> tok
//	team  <sessions> sessions  <checkpoints> checkpoints  <tokens> tok
//
//	top  <TopAgent> · <TopSkill> · <TopLabel> · <TopModel>
//
//	<AgentCount> agents · <RepoCount> repos · <ActiveDays> active days
func renderSummaryBand(s SummaryBand, styles Styles) string {
	var b strings.Builder

	// Title line.
	b.WriteString(styles.title.Render(s.RangeLabel))
	b.WriteString("\n\n")

	// you / team rows.
	b.WriteString(renderSummaryRow("you", styles.accent,
		s.YouSessions, s.YouCheckpoints, s.YouTokens, styles))
	b.WriteString("\n")
	b.WriteString(renderSummaryRow("team", styles.team,
		s.TeamSessions, s.TeamCheckpoints, s.TeamTokens, styles))

	// top line (only when at least one signal is non-empty).
	topLine := renderTopLine(s, styles)
	if topLine != "" {
		b.WriteString("\n\n")
		b.WriteString(topLine)
	}

	// Context line.
	b.WriteString("\n\n")
	b.WriteString(renderContextLine(s, styles))
	return b.String()
}

func renderSummaryRow(label string, labelSty lipgloss.Style, sessions, checkpoints, tokens int, styles Styles) string {
	return fmt.Sprintf("%s  %s  %s  %s",
		labelSty.Render(label),
		styles.value.Render(fmt.Sprintf("%d sessions", sessions)),
		styles.value.Render(fmt.Sprintf("%d checkpoints", checkpoints)),
		styles.value.Render(formatTokens(tokens)+" tok"))
}

func renderTopLine(s SummaryBand, styles Styles) string {
	var parts []string
	if s.TopAgent != "" {
		parts = append(parts, styles.accent.Render(s.TopAgent))
	}
	if s.TopSkill != "" {
		parts = append(parts, styles.info.Render(s.TopSkill))
	}
	if s.TopLabel != "" {
		parts = append(parts, labelStyle(s.TopLabel, styles).Render(s.TopLabel))
	}
	if s.TopModel != "" {
		parts = append(parts, styles.value.Render(s.TopModel))
	}
	if len(parts) == 0 {
		return ""
	}
	return styles.label.Render("top") + "  " + strings.Join(parts, " · ")
}

func renderContextLine(s SummaryBand, styles Styles) string {
	return styles.muted.Render(fmt.Sprintf("%d agents · %d repos · %d active days",
		s.AgentCount, s.RepoCount, s.ActiveDays))
}

// labelStyle returns the semantic color style for a canonical label name.
// Unknown labels fall through to styles.value (white-bold) — new labels can
// ship before their color is added (spec §Summary panel §Unknown label fallback).
func labelStyle(name string, s Styles) lipgloss.Style {
	switch name {
	case "feature_build", "enhancement": //nolint:goconst // label names are semantic identifiers, not code duplication
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	case "bug_fix", "security_fix":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	case "refactor", "optimization":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	case "testing":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("170"))
	case "investigation", "documentation":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	case "performance":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	default:
		return s.value
	}
}
