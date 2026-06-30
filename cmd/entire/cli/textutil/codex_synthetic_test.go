package textutil

import "testing"

func TestIsCodexSyntheticContent(t *testing.T) {
	t.Parallel()
	synthetic := []string{
		"<environment_context>\n<cwd>/repo</cwd>\n</environment_context>",
		"<subagent_notification>\n{\"agent_path\":\"019e0000\"}",
		"<turn_aborted>",
		"<permissions>read</permissions>",
		"<collaboration_mode>",
		"<skills_instructions>",
		"# AGENTS.md\nProject rules",
		"   \n<environment_context>", // leading whitespace still matches
	}
	for _, s := range synthetic {
		if !IsCodexSyntheticContent(s) {
			t.Errorf("expected synthetic, got user-authored: %q", s)
		}
	}

	userAuthored := []string{
		"Fix the login bug",
		"prompt it to spawn a subagent that edits a file",
		"please update <environment_context> handling in the parser", // mentions tag mid-text
		"",
	}
	for _, s := range userAuthored {
		if IsCodexSyntheticContent(s) {
			t.Errorf("expected user-authored, got synthetic: %q", s)
		}
	}
}
