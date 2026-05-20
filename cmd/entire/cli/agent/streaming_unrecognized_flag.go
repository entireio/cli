package agent

import "strings"

// LooksLikeUnrecognizedFlag reports whether stderr matches the canonical
// CLI-rejected-a-flag pattern: a rejection phrase ("unknown flag",
// "unrecognized option", etc.) combined with at least one of the
// caller-specified flag-name keywords.
//
// Used by streaming text generators to detect when an older CLI version
// rejects a streaming-mode argv so the caller can fall back to a
// non-streaming path. Returns false if no rejection phrase is present or
// no keyword matches.
//
// Keyword matching is case-insensitive and substring-based; pass the
// distinguishing words from your streaming flags (e.g. "stream-json",
// "output-format") rather than the leading "--".
func LooksLikeUnrecognizedFlag(stderr string, flagKeywords ...string) bool {
	lower := strings.ToLower(stderr)
	rejection := strings.Contains(lower, "unknown flag") ||
		strings.Contains(lower, "unrecognized option") ||
		strings.Contains(lower, "unknown option") ||
		strings.Contains(lower, "invalid option")
	if !rejection {
		return false
	}
	for _, kw := range flagKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
