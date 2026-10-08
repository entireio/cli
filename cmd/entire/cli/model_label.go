package cli

import (
	"regexp"
	"strings"
)

// claudeModelRe matches Claude identifiers: claude-<family>-<major>[-<tail>].
// The tail (rest) carries optional minor versions and/or a legacy date suffix.
var claudeModelRe = regexp.MustCompile(`(?i)^claude-(opus|sonnet|haiku)-(\d+)(.*)$`)

// dateSuffixRe matches a legacy date chunk (6+ digits, e.g. 20250514).
var dateSuffixRe = regexp.MustCompile(`^\d{6,}$`)

// formatModel turns a raw model identifier into a short display label, mirroring
// entire.io's frontend formatModel (frontend/src/lib/model.ts) so the CLI's
// session list reads identically to the web Overview page. Examples:
//
//	"claude-opus-4-6"          -> "Opus 4.6"
//	"claude-sonnet-4-20250514" -> "Sonnet 4"
//	"gpt-4o"                   -> "GPT-4o"
//	"gemini-2.0-flash"         -> "Gemini 2.0 Flash"
//
// Unknown formats pass through unchanged. Empty/whitespace input returns ""
// (the web returns null; the CLI renders nothing for it).
func formatModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}

	// Claude: title-case family, then major[.minor], dropping any date suffix.
	if m := claudeModelRe.FindStringSubmatch(trimmed); m != nil {
		family, major, rest := m[1], m[2], m[3]
		familyLabel := strings.ToUpper(family[:1]) + strings.ToLower(family[1:])
		var minorParts []string
		for _, part := range strings.Split(rest, "-") {
			if part == "" {
				continue
			}
			// A 6+ digit chunk is a date suffix — ignore it and everything after.
			if dateSuffixRe.MatchString(part) {
				break
			}
			minorParts = append(minorParts, part)
		}
		if minor := strings.Join(minorParts, "."); minor != "" {
			return familyLabel + " " + major + "." + minor
		}
		return familyLabel + " " + major
	}

	// GPT: gpt-4o -> "GPT-4o" (case-insensitive prefix; "gpt-" is 4 ASCII bytes).
	if len(trimmed) >= 4 && strings.EqualFold(trimmed[:4], "gpt-") {
		return "GPT-" + trimmed[4:]
	}

	// Gemini: gemini-2.0-flash -> "Gemini 2.0 Flash" (upper-first each part).
	if strings.HasPrefix(strings.ToLower(trimmed), "gemini-") {
		parts := strings.Split(trimmed, "-")
		for i, part := range parts {
			parts[i] = upperFirst(part)
		}
		return strings.Join(parts, " ")
	}

	return trimmed
}

// upperFirst upper-cases the first rune of s, leaving the remainder unchanged
// (matching the web's `part.charAt(0).toUpperCase() + part.slice(1)`).
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
