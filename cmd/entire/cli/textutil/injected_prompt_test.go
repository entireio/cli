package textutil

import "testing"

// TestIsInjectedPrompt_RealRolloutShapes covers every first-user-item shape
// observed across 164 real Codex rollouts. The two dominant shapes (an AGENTS.md
// dump and <environment_context>) account for 153 of them; the remaining 11 are
// <recommended_plugins>, <user_action>, and <turn_aborted>, each of which would
// otherwise become a session title.
func TestIsInjectedPrompt_RealRolloutShapes(t *testing.T) {
	t.Parallel()

	injected := map[string]string{
		"AGENTS.md dump with path":  "# AGENTS.md instructions for /Users/soph/Work/repo\n\n<INSTRUCTIONS>\nfollow them\n</INSTRUCTIONS>",
		"AGENTS.md dump bare":       "# AGENTS.md\nThese are the project instructions.",
		"environment context":       "<environment_context>\n  <cwd>/repo</cwd>\n  <shell>zsh</shell>\n</environment_context>",
		"recommended plugins":       "<recommended_plugins>\nHere is a list of plugins that are available but not installed.\n\n- Airtable\n",
		"user action review result": "<user_action>\n  <context>User initiated a review task.</context>\n  <action>review</action>\n</user_action>",
		"turn aborted":              "<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>",
		"subagent notification":     "<subagent_notification>\n{\"agent_path\":\"0\"}\n</subagent_notification>",
		"permissions instructions":  "<permissions instructions>\nYou are sandboxed.\n</permissions instructions>",
		"skills instructions":       "<skills_instructions>\nuse them\n</skills_instructions>",
		"collaboration mode":        "<collaboration_mode>\npair\n</collaboration_mode>",
		"apps instructions":         "<apps_instructions>\napps\n</apps_instructions>",
		"plugins instructions":      "<plugins_instructions>\nplugins\n</plugins_instructions>",
		"instructions tag first":    "<INSTRUCTIONS>\nAGENTS.md instructions follow\n</INSTRUCTIONS>",
		"leading whitespace":        "\n  <environment_context>\n  <cwd>/repo</cwd>\n</environment_context>",
	}
	for name, text := range injected {
		t.Run("injected/"+name, func(t *testing.T) {
			t.Parallel()
			if !IsInjectedPrompt(text) {
				t.Errorf("IsInjectedPrompt(%q) = false, want true", text)
			}
		})
	}

	// Genuine prompts, including ones that discuss the injected markers. The
	// match is anchored precisely so these stay prompts: misclassifying one
	// costs the checkpoint its title.
	genuine := map[string]string{
		"plain":                      "fix the login bug",
		"mentions INSTRUCTIONS tag":  "why does the AGENTS.md instructions block get wrapped in <INSTRUCTIONS> tags?",
		"mentions environment tag":   "the <environment_context> block is leaking into titles — can you fix that?",
		"mentions AGENTS.md midway":  "please update AGENTS.md to document the new flag",
		"mentions turn_aborted":      "what emits <turn_aborted> and should we count it as a turn?",
		"code fence with the marker": "```\n<environment_context>\n```\nwhat is this?",
		"empty":                      "",
	}
	for name, text := range genuine {
		t.Run("genuine/"+name, func(t *testing.T) {
			t.Parallel()
			if IsInjectedPrompt(text) {
				t.Errorf("IsInjectedPrompt(%q) = true, want false", text)
			}
		})
	}
}
