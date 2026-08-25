// Package gitpath compares repository-relative paths using filesystem
// equivalences that can collapse distinct Git names onto one working-tree
// file.
package gitpath

import "strings"

// Equivalent reports whether two slash-separated Git paths can name the same
// working-tree file after case folding and Win32 trailing-dot/space stripping.
func Equivalent(a, b string) bool {
	return strings.EqualFold(normalizeForFilesystem(a), normalizeForFilesystem(b))
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
