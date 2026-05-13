// Package review — see env.go for package-level rationale.
package review

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func stripANSI(s string) string {
	return ansi.Strip(s)
}

func sanitizeDisplayText(s string) string {
	stripped := stripANSI(s)
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

func padDisplayWidth(s string, width int) string {
	return padDisplayWidthWith(s, width, " ")
}

func padDisplayWidthWith(s string, width int, pad string) string {
	s = truncateDisplayWidth(s, width)
	remaining := width - ansi.StringWidth(s)
	if remaining <= 0 {
		return s
	}
	if ansi.StringWidth(pad) != 1 {
		return s + strings.Repeat(" ", remaining)
	}
	return s + strings.Repeat(pad, remaining)
}

func truncateDisplayWidth(s string, width int) string {
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

// wrapDisplayWidth wraps s to lines no wider than width display cells. Embedded
// '\n' characters are honored as paragraph boundaries: each paragraph is
// sanitized (ANSI/control stripped) and wrapped independently. A paragraph that
// wraps to nothing still contributes an empty line, preserving blank-line
// structure between paragraphs.
//
// Trailing newlines are stripped before splitting so "text\n" yields a single
// line, not a phantom blank tail — matching how splitBodyToHeight trims its
// input.
//
// Returns nil for width <= 0 or input that is empty (or only newlines).
func wrapDisplayWidth(s string, width int) []string {
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
		clean := sanitizeDisplayText(p)
		if clean == "" {
			out = append(out, "")
			continue
		}
		wrapped := ansi.Wrap(clean, width, "")
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	return out
}
