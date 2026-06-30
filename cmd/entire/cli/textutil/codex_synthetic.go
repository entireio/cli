package textutil

import "strings"

// codexSyntheticContentPrefixes are the leading markers of Codex content blocks
// that the CLI injects into the rollout as user-role messages but which are NOT
// user-authored prompts: environment context, AGENTS.md instructions, permission
// notices, collaboration/skills preamble, turn-abort markers, and subagent
// completion notifications. They must be excluded anywhere we surface the user's
// actual prompt (status line, commit messages, compacted transcript).
var codexSyntheticContentPrefixes = []string{
	"<permissions",
	"<collaboration_mode>",
	"<skills_instructions>",
	"<environment_context>",
	"<turn_aborted>",
	"<subagent_notification>",
	"# AGENTS.md",
}

// IsCodexSyntheticContent reports whether a Codex user-message content block is
// system-injected rather than user-authored. It matches against the leading
// (whitespace-trimmed) marker so an enclosing tag like
// "<subagent_notification>\n{...}" is recognized regardless of trailing content.
func IsCodexSyntheticContent(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, p := range codexSyntheticContentPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}
