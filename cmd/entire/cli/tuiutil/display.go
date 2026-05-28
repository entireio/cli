// Package tuiutil hosts width-aware text helpers for fixed-width TUI
// dashboards: ANSI/control-char stripping, display-width-based truncation
// and padding, and a compact duration formatter.
package tuiutil

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// StripANSI removes ANSI escape sequences from s.
func StripANSI(s string) string {
	return ansi.Strip(s)
}

// SanitizeDisplayText strips ANSI escapes and control characters so the
// result is safe to render in a single-line table cell. Newlines and tabs
// collapse to a single space; carriage returns and other control runes are
// dropped entirely.
func SanitizeDisplayText(s string) string {
	stripped := StripANSI(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return ' '
		case '\r':
			return -1
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, stripped)
}

// PadDisplayWidth truncates or right-pads s with spaces so its display
// width is exactly width cells (ANSI-aware).
func PadDisplayWidth(s string, width int) string {
	return PadDisplayWidthWith(s, width, " ")
}

// PadDisplayWidthWith truncates or right-pads s with the pad string so its
// display width is exactly width cells. Pad strings whose display width is
// not 1 fall back to space padding to keep alignment predictable.
func PadDisplayWidthWith(s string, width int, pad string) string {
	s = TruncateDisplayWidth(s, width)
	remaining := width - ansi.StringWidth(s)
	if remaining <= 0 {
		return s
	}
	if ansi.StringWidth(pad) != 1 {
		return s + strings.Repeat(" ", remaining)
	}
	return s + strings.Repeat(pad, remaining)
}

// TruncateDisplayWidth shortens s so its display width is at most width
// cells, appending "…" as an ellipsis when truncation happens. Width 0 or
// less returns the empty string; width 1 truncates without an ellipsis
// since the ellipsis would consume the whole budget.
func TruncateDisplayWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return ansi.Truncate(s, width, "")
	}
	return ansi.Truncate(s, width, "…")
}

// WrapDisplayWidth wraps s to lines no wider than width display cells.
// Embedded '\n' characters are honored as paragraph boundaries: each
// paragraph is sanitized (ANSI/control stripped) and wrapped independently.
// A paragraph that wraps to nothing still contributes an empty line,
// preserving blank-line structure between paragraphs.
//
// Trailing newlines are stripped before splitting so "text\n" yields a
// single line, not a phantom blank tail.
//
// Returns nil for width <= 0 or input that is empty (or only newlines).
func WrapDisplayWidth(s string, width int) []string {
	if width <= 0 {
		return nil
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	paragraphs := strings.Split(s, "\n")
	out := make([]string, 0, len(paragraphs))
	for _, p := range paragraphs {
		clean := SanitizeDisplayText(p)
		if clean == "" {
			out = append(out, "")
			continue
		}
		wrapped := ansi.Wrap(clean, width, "")
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	return out
}

// FormatDuration renders a time.Duration in compact form for TUI
// dashboards: "523ms" / "8.4s" / "1m42s".
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}
