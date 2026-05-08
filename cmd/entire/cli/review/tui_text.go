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
