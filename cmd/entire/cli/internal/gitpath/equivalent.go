// Package gitpath compares repository-relative paths using filesystem
// equivalences that can collapse distinct Git names onto one working-tree
// file.
package gitpath

import (
	"strings"
	"unicode"
)

// Equivalent reports whether two slash-separated Git paths can name the same
// working-tree file after case folding and Win32 trailing-dot/space stripping.
func Equivalent(a, b string) bool {
	return CanonicalKey(a) == CanonicalKey(b)
}

// CanonicalKey returns a stable comparison key for a slash-separated Git
// path under the same filesystem equivalences as Equivalent.
func CanonicalKey(path string) string {
	return strings.Map(canonicalFoldRune, normalizeForFilesystem(path))
}

func normalizeForFilesystem(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if trimmed := strings.TrimRight(part, ". "); trimmed != "" {
			parts[i] = trimmed
		}
	}
	return strings.Join(parts, "/")
}

func canonicalFoldRune(r rune) rune {
	minimum := r
	for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
		if folded < minimum {
			minimum = folded
		}
	}
	return minimum
}
