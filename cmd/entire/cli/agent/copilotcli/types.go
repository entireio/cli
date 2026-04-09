package copilotcli

// CopilotHooksFile represents the .github/hooks/entire.json structure.
// Copilot CLI uses a flat JSON file with version and hooks sections.
// All JSON files in .github/hooks/ are auto-discovered by the Copilot CLI.
//

type CopilotHooksFile struct {
	Version int          `json:"version"`
	Hooks   CopilotHooks `json:"hooks"`
}

// CopilotHooks contains all hook configurations using camelCase keys.
//

type CopilotHooks struct {
	UserPromptSubmitted []CopilotHookEntry `json:"userPromptSubmitted,omitempty"`
	SessionStart        []CopilotHookEntry `json:"sessionStart,omitempty"`
	AgentStop           []CopilotHookEntry `json:"agentStop,omitempty"`
	SessionEnd          []CopilotHookEntry `json:"sessionEnd,omitempty"`
	SubagentStop        []CopilotHookEntry `json:"subagentStop,omitempty"`
	PreToolUse          []CopilotHookEntry `json:"preToolUse,omitempty"`
	PostToolUse         []CopilotHookEntry `json:"postToolUse,omitempty"`
	ErrorOccurred       []CopilotHookEntry `json:"errorOccurred,omitempty"`
}

// CopilotHookEntry represents a single hook command.
// Copilot CLI hooks have a type field ("command") and a bash field for the command string.
// Optional fields (cwd, timeoutSec, env, comment) are preserved on round-trip.

type CopilotHookEntry struct {
	Type       string            `json:"type"`
	Bash       string            `json:"bash"`
	Comment    string            `json:"comment,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	TimeoutSec int               `json:"timeoutSec,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}
