package textutil

import "strings"

// codexSyntheticTagPrefixes are the leading XML-style markers of Codex content
// blocks the CLI injects into the rollout as user-role messages but which are NOT
// user-authored prompts: environment context, permission notices,
// collaboration/skills preamble, turn-abort markers, and subagent completion
// notifications. Each is a tag start a real prompt effectively never begins with,
// so a prefix match is safe. (AGENTS.md is handled separately — see below.)
var codexSyntheticTagPrefixes = []string{
	"<permissions",
	"<collaboration_mode>",
	"<skills_instructions>",
	"<environment_context>",
	"<turn_aborted>",
	"<subagent_notification>",
}

// codexAgentsMDHeading is how Codex injects AGENTS.md: a markdown H1 block,
// always followed by the file body on the next line. Because it is an ordinary
// markdown heading of a real filename, it must be anchored to that injected form
// (exact, or heading + line break) — a bare prefix match would wrongly drop a
// genuine user prompt that merely starts with "# AGENTS.md ..." (e.g. asking to
// edit AGENTS.md), emptying the commit message / status prompt.
const codexAgentsMDHeading = "# AGENTS.md"

// IsCodexSyntheticContent reports whether a Codex user-message content block is
// system-injected rather than user-authored. It matches the leading
// (whitespace-trimmed) marker so an enclosing tag like
// "<subagent_notification>\n{...}" is recognized regardless of trailing content.
func IsCodexSyntheticContent(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, p := range codexSyntheticTagPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	// AGENTS.md: only the injected form (exact heading, or heading immediately
	// followed by a line break), never a user prompt that opens with those words.
	if trimmed == codexAgentsMDHeading ||
		strings.HasPrefix(trimmed, codexAgentsMDHeading+"\n") ||
		strings.HasPrefix(trimmed, codexAgentsMDHeading+"\r\n") {
		return true
	}
	return false
}
