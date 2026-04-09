package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/entireio/cli/cmd/entire/cli/agent"

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

// isTerminalWriter returns true if the writer is connected to a terminal.
func isTerminalWriter(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		return term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: uintptr->int is safe for fd
	}
	return false
}

// shouldUseColor returns true if the writer supports color output.
func shouldUseColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTerminalWriter(w)
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
