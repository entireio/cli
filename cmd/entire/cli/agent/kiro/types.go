package kiro

// KiroHooksFile represents the .kiro/hooks.json structure.
// Kiro uses a flat JSON file with hooks sections, similar to Cursor.
//
//nolint:revive // KiroHooksFile is clearer than HooksFile when used outside this package
type KiroHooksFile struct {
	Hooks KiroHooks `json:"hooks"`
}

// KiroHooks contains all hook configurations.
//
//nolint:revive // KiroHooks is clearer than Hooks when used outside this package
type KiroHooks struct {
	AgentSpawn       []KiroHookEntry `json:"agentSpawn,omitempty"`
	UserPromptSubmit []KiroHookEntry `json:"userPromptSubmit,omitempty"`
	Stop             []KiroHookEntry `json:"stop,omitempty"`
	PreToolUse       []KiroHookEntry `json:"preToolUse,omitempty"`
	PostToolUse      []KiroHookEntry `json:"postToolUse,omitempty"`
}

// KiroHookEntry represents a single hook command.
//
//nolint:revive // KiroHookEntry is clearer than HookEntry when used outside this package
type KiroHookEntry struct {
	Command string `json:"command"`
	Matcher string `json:"matcher,omitempty"`
}

// agentSpawnRaw is the JSON structure from Kiro's agentSpawn hook.
type agentSpawnRaw struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
}

// userPromptSubmitRaw is the JSON structure from Kiro's userPromptSubmit hook.
type userPromptSubmitRaw struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	Prompt        string `json:"prompt"`
}

// stopRaw is the JSON structure from Kiro's stop hook.
type stopRaw struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
}

// preToolUseRaw is the JSON structure from Kiro's preToolUse hook.
type preToolUseRaw struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	ToolName      string `json:"tool_name"`
	ToolUseID     string `json:"tool_use_id"`
	Input         string `json:"input"`
}

// postToolUseRaw is the JSON structure from Kiro's postToolUse hook.
type postToolUseRaw struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	ToolName      string `json:"tool_name"`
	ToolUseID     string `json:"tool_use_id"`
	Output        string `json:"output"`
}
