package textutil

import "strings"

// injectedPromptPrefixes are the leading markers of content an agent injects
// into the user side of a transcript: runtime preambles, instruction-file dumps,
// and tool/notification results replayed as user turns. None of it is
// user-authored, so display paths skip it when picking a title and turn counts
// exclude it.
//
// The list is anchored (prefix-only) on purpose: a genuine prompt that merely
// mentions one of these tags must not be misclassified.
//
// Derived from 164 real Codex rollouts: the first user-role item is an AGENTS.md
// dump (107) or <environment_context> (46) in 153 of them, with
// <recommended_plugins> (8), <user_action> (2), and <turn_aborted> (1) making up
// the remainder. The developer-role markers (<permissions instructions>,
// <skills_instructions>, <collaboration_mode>, <apps_instructions>,
// <plugins_instructions>) never reach the prompt path — Codex's ExtractPrompts
// filters by role — but transcript compaction sees them, so they belong here too.
var injectedPromptPrefixes = []string{
	"# AGENTS.md",             // instruction-file dump: bare, or "…instructions for <path>"
	"<environment_context>",   // runtime preamble: cwd, shell, sandbox
	"<permissions",            // "<permissions instructions>"
	"<collaboration_mode>",    //
	"<skills_instructions>",   //
	"<apps_instructions>",     //
	"<plugins_instructions>",  //
	"<recommended_plugins>",   // catalog of uninstalled plugins
	"<user_action>",           // review/tool output replayed as a user turn
	"<subagent_notification>", // subagent lifecycle notice
	"<turn_aborted>",          // interrupted-turn notice
}

// IsInjectedPrompt reports whether text is agent-injected rather than
// user-authored. It is the single source of truth for that question: callers
// that pick a session title, count user turns, or compact a transcript all use
// it so the classification cannot drift between them.
func IsInjectedPrompt(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range injectedPromptPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	// Some instruction injections lead with the wrapper tag rather than the
	// heading. Require the AGENTS.md marker as well, and keep the tag check
	// anchored, so a genuine prompt asking about <INSTRUCTIONS> stays a prompt.
	return strings.HasPrefix(trimmed, "<INSTRUCTIONS>") && strings.Contains(trimmed, "AGENTS.md instructions")
}
