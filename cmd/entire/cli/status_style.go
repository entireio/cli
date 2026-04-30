package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"

	"golang.org/x/term"
)

// statusStyles holds pre-built lipgloss styles and terminal metadata.
type statusStyles struct {
	colorEnabled bool
	width        int

	// Styles
	green  lipgloss.Style
	red    lipgloss.Style
	gray   lipgloss.Style
	bold   lipgloss.Style
	dim    lipgloss.Style
	agent  lipgloss.Style // amber/orange for agent names
	cyan   lipgloss.Style
	yellow lipgloss.Style // yellow for stale warnings
}

// newStatusStyles creates styles appropriate for the output writer.
func newStatusStyles(w io.Writer) statusStyles {
	useColor := shouldUseColor(w)
	width := getTerminalWidth(w)

	s := statusStyles{
		colorEnabled: useColor,
		width:        width,
	}

	if useColor {
		s.green = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
		s.red = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
		s.gray = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		s.bold = lipgloss.NewStyle().Bold(true)
		s.dim = lipgloss.NewStyle().Faint(true)
		s.agent = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
		s.cyan = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
		s.yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	}

	return s
}

// render applies a style to text only when color is enabled.
func (s statusStyles) render(style lipgloss.Style, text string) string {
	if !s.colorEnabled {
		return text
	}
	return style.Render(text)
}

// shouldUseColor returns true if the writer supports color output.
func shouldUseColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return interactive.IsTerminalWriter(w)
}

// getTerminalWidth returns the terminal width, capped at 80 with a fallback of 60.
// It first checks the writer itself, then falls back to Stdout/Stderr.
func getTerminalWidth(w io.Writer) int {
	// Try the output writer first
	if f, ok := w.(*os.File); ok {
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 { //nolint:gosec // G115: uintptr->int is safe for fd
			return min(width, 80)
		}
	}

	// Fall back to Stdout, then Stderr
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if f == nil {
			continue
		}
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 { //nolint:gosec // G115: uintptr->int is safe for fd
			return min(width, 80)
		}
	}

	return 60
}

// formatTokenCount formats a token count for display.
// 0 → "0", 500 → "500", 1200 → "1.2k", 14300 → "14.3k"
func formatTokenCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	f := float64(n) / 1000.0
	s := fmt.Sprintf("%.1f", f)
	// Remove trailing ".0" for clean display (e.g., 1000 → "1k" not "1.0k")
	s = strings.TrimSuffix(s, ".0")
	return s + "k"
}

// totalTokens recursively sums all token fields including subagent tokens.
func totalTokens(tu *agent.TokenUsage) int {
	if tu == nil {
		return 0
	}
	total := tu.InputTokens + tu.CacheCreationTokens + tu.CacheReadTokens + tu.OutputTokens
	total += totalTokens(tu.SubagentTokens)
	return total
}

// explainRow is one entry in a metadata block: dim label + plain value.
type explainRow struct {
	Label string
	Value string
}

// identityBullet renders "● <label> <id>\n". Bullet is brand orange when color
// is enabled; ID is also orange to mirror the existing checkpoint header. When
// id is empty (e.g., temporary checkpoints append "[temporary]" to the label
// instead of using an id slot), the trailing space + id is suppressed.
func (s statusStyles) identityBullet(label, id string) string {
	if id == "" {
		if !s.colorEnabled {
			return fmt.Sprintf("● %s\n", label)
		}
		bullet := s.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")), "●")
		return fmt.Sprintf("%s %s\n", bullet, s.render(s.bold, label))
	}
	if !s.colorEnabled {
		return fmt.Sprintf("● %s %s\n", label, id)
	}
	bullet := s.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")), "●")
	idStyled := s.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")), id)
	return fmt.Sprintf("%s %s %s\n", bullet, s.render(s.bold, label), idStyled)
}

// listIdentityBullet renders "● <id>  <suffix>\n" — orange bullet, bold-orange ID,
// then plain (or dimmed) suffix. Used by the explain list view where the ID is
// the primary identifier (different ordering from identityBullet, which puts
// the static label first).
func (s statusStyles) listIdentityBullet(id, suffix string) string {
	if !s.colorEnabled {
		if suffix == "" {
			return fmt.Sprintf("● %s\n", id)
		}
		return fmt.Sprintf("● %s  %s\n", id, suffix)
	}
	bullet := s.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")), "●")
	idStyled := s.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")).Bold(true), id)
	if suffix == "" {
		return fmt.Sprintf("%s %s\n", bullet, idStyled)
	}
	return fmt.Sprintf("%s %s  %s\n", bullet, idStyled, suffix)
}

// successBullet renders "✓ <label>\n" — green checkmark + bold label.
func (s statusStyles) successBullet(label string) string {
	if !s.colorEnabled {
		return fmt.Sprintf("✓ %s\n", label)
	}
	return fmt.Sprintf("%s %s\n", s.render(s.green, "✓"), s.render(s.bold, label))
}

// failureBullet renders "✗ <label>\n" — red ✗ + bold label.
func (s statusStyles) failureBullet(label string) string {
	if !s.colorEnabled {
		return fmt.Sprintf("✗ %s\n", label)
	}
	return fmt.Sprintf("%s %s\n", s.render(s.red, "✗"), s.render(s.bold, label))
}

// renderIdentity returns identityBullet + metadataRows + horizontalRule + trailing newline.
// This is the exact shape used by formatCheckpointHeader; rendering an entire
// header block is one call instead of three.
func (s statusStyles) renderIdentity(label, id string, rows []explainRow) string {
	var b strings.Builder
	b.WriteString(s.identityBullet(label, id))
	b.WriteString(s.metadataRows(rows))
	b.WriteString(s.horizontalRule(s.width))
	b.WriteString("\n")
	return b.String()
}

// renderSuccess returns successBullet + metadataRows. No trailing rule (success
// blocks are short and don't precede a body section).
func (s statusStyles) renderSuccess(label string, rows []explainRow) string {
	if len(rows) == 0 {
		return s.successBullet(label)
	}
	return s.successBullet(label) + s.metadataRows(rows)
}

// renderFailure returns failureBullet + metadataRows. Used when a command
// prints a styled error block to errW before returning *SilentError.
func (s statusStyles) renderFailure(label string, rows []explainRow) string {
	if len(rows) == 0 {
		return s.failureBullet(label)
	}
	return s.failureBullet(label) + s.metadataRows(rows)
}

// metadataRow renders a single key/value row using a 7-char min-padded label.
// 7-char-pad + 2-space-gutter is visually equivalent to the existing
// formatCheckpointHeader's %-9s + no-gutter (see explain.go) for any label up
// to 9 chars, and the explicit gutter scales cleanly when a longer label
// (e.g. "checkpoints", 11 chars) is present.
//
// Use metadataRows for multi-row blocks where alignment depends on the
// widest label.
func (s statusStyles) metadataRow(label, value string) string {
	width := 7
	if l := len(label); l > width {
		width = l
	}
	return s.metadataRowsWithWidth([]explainRow{{Label: label, Value: value}}, width)
}

// metadataRows joins rows with consistent label-column width:
// max(7, widest label in slice). See metadataRow for the 7 vs 9
// equivalence note.
func (s statusStyles) metadataRows(rows []explainRow) string {
	width := 7
	for _, r := range rows {
		if l := len(r.Label); l > width {
			width = l
		}
	}
	return s.metadataRowsWithWidth(rows, width)
}

// metadataRowsWithWidth is the underlying renderer used by metadataRow and
// metadataRows. The caller computes the column width; this function owns the
// byte layout (2-space indent, padded label, 2-space gutter, value, newline)
// and the empty-label continuation branch (4-space hanging indent, no dim
// styling — used for multi-line items beneath a parent label like "causes").
//
// The min=7 default chosen by metadataRow/metadataRows produces output
// identical to formatCheckpointHeader's %-9s + no-gutter rendering at
// explain.go for any label up to 9 chars.
func (s statusStyles) metadataRowsWithWidth(rows []explainRow, width int) string {
	var b strings.Builder
	for _, r := range rows {
		// Empty-label rows are continuation lines (e.g., bullet items under a "causes" row).
		// They render with a 4-space hanging indent and no dim styling.
		if r.Label == "" {
			fmt.Fprintf(&b, "    %s\n", r.Value)
			continue
		}
		paddedLabel := fmt.Sprintf("%-*s", width, r.Label)
		if s.colorEnabled {
			paddedLabel = s.render(s.dim, paddedLabel)
		}
		// Two-space indent + padded label + two-space gutter + value + newline.
		fmt.Fprintf(&b, "  %s  %s\n", paddedLabel, r.Value)
	}
	return b.String()
}

// horizontalRule renders a dimmed horizontal rule of the given width.
func (s statusStyles) horizontalRule(width int) string {
	rule := strings.Repeat("─", width)
	return s.render(s.dim, rule)
}

// sectionRule renders a section header like: ── Active Sessions ────────────
func (s statusStyles) sectionRule(label string, width int) string {
	prefix := "── "
	content := label + " "
	usedWidth := len([]rune(prefix)) + len([]rune(content))
	trailing := width - usedWidth
	if trailing < 1 {
		trailing = 1
	}

	var b strings.Builder
	b.WriteString(s.render(s.dim, "── "))
	b.WriteString(s.render(s.dim, label))
	b.WriteString(" ")
	b.WriteString(s.render(s.dim, strings.Repeat("─", trailing)))
	return b.String()
}

// activeTimeDisplay formats a last interaction time for display.
// Returns "active now" for recent activity (<1min), otherwise "active Xm ago".
func activeTimeDisplay(lastInteraction *time.Time) string {
	if lastInteraction == nil {
		return ""
	}
	d := time.Since(*lastInteraction)
	if d < time.Minute {
		return "active now"
	}
	return "active " + timeAgo(*lastInteraction)
}
