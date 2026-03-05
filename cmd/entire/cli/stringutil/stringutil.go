// Package stringutil provides UTF-8 safe string manipulation utilities.
package stringutil

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// whitespaceRegex matches one or more whitespace characters (including newlines)
var whitespaceRegex = regexp.MustCompile(`\s+`)

// CollapseWhitespace replaces sequences of whitespace (including newlines, tabs)
// with a single space and trims leading/trailing whitespace.
// Useful for preparing multi-line text for single-line display.
func CollapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRegex.ReplaceAllString(s, " "))
}

// TruncateRunes truncates a string to at most maxRunes runes, appending suffix if truncated.
// This is safe for multi-byte UTF-8 characters unlike byte-based slicing.
func TruncateRunes(s string, maxRunes int, suffix string) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	suffixRunes := []rune(suffix)
	truncateAt := maxRunes - len(suffixRunes)
	if truncateAt < 0 {
		truncateAt = 0
	}
	return string(runes[:truncateAt]) + suffix
}

// CapitalizeFirst capitalizes the first rune of a string.
// This is safe for multi-byte UTF-8 characters unlike byte indexing.
func CapitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// UniqueStrings deduplicates a string slice, preserving order and skipping empty strings.
func UniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}
