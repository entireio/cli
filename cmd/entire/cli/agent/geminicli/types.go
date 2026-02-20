package geminicli

// GeminiSettings represents the .gemini/settings.json structure
type GeminiSettings struct {
	HooksConfig GeminiHooksConfig `json:"hooksConfig,omitempty"`
	Hooks       GeminiHooks       `json:"hooks,omitempty"`
}

// GeminiHooksConfig contains tool-related settings
type GeminiHooksConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// GeminiHooks contains all hook configurations
type GeminiHooks struct {
	// Hooks are only executed when hooksConfig.enabled is true in .gemini/settings.json.
	SessionStart        []GeminiHookMatcher `json:"SessionStart,omitempty"`
	SessionEnd          []GeminiHookMatcher `json:"SessionEnd,omitempty"`
	BeforeAgent         []GeminiHookMatcher `json:"BeforeAgent,omitempty"`
	AfterAgent          []GeminiHookMatcher `json:"AfterAgent,omitempty"`
	BeforeModel         []GeminiHookMatcher `json:"BeforeModel,omitempty"`
	AfterModel          []GeminiHookMatcher `json:"AfterModel,omitempty"`
	BeforeToolSelection []GeminiHookMatcher `json:"BeforeToolSelection,omitempty"`
	BeforeTool          []GeminiHookMatcher `json:"BeforeTool,omitempty"`
	AfterTool           []GeminiHookMatcher `json:"AfterTool,omitempty"`
	PreCompress         []GeminiHookMatcher `json:"PreCompress,omitempty"`
	Notification        []GeminiHookMatcher `json:"Notification,omitempty"`
}

// GeminiHookMatcher matches hooks to specific patterns
type GeminiHookMatcher struct {
	Matcher string            `json:"matcher,omitempty"`
	Hooks   []GeminiHookEntry `json:"hooks"`
}

// GeminiHookEntry represents a single hook command.
// Unlike Claude Code, Gemini CLI requires a "name" field for each hook entry.
type GeminiHookEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Command string `json:"command"`
}

// sessionInfoRaw is the JSON structure from SessionStart/SessionEnd hooks
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Timestamp      string `json:"timestamp"`
	Source         string `json:"source,omitempty"` // For SessionStart: startup, resume, clear
	Reason         string `json:"reason,omitempty"` // For SessionEnd: exit, logout
}

// agentHookInputRaw is the JSON structure from BeforeAgent/AfterAgent hooks.
// BeforeAgent includes the user's prompt, similar to Claude's UserPromptSubmit.
type agentHookInputRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Timestamp      string `json:"timestamp"`
	Prompt         string `json:"prompt,omitempty"` // User's prompt (BeforeAgent only)
}

// Tool names used in Gemini CLI that modify files
// Note: Gemini CLI uses different names in different contexts:
// - Internal/transcript names: write_file, replace
// - Display names: WriteFile, Edit
const (
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"
	ToolSaveFile  = "save_file"
	ToolReplace   = "replace"
)

// FileModificationTools lists tools that create or modify files in Gemini CLI
var FileModificationTools = []string{
	ToolWriteFile,
	ToolEditFile,
	ToolSaveFile,
	ToolReplace,
}

// geminiMessageTokens represents token usage from a Gemini API response.
// This is specific to Gemini's API format where each message has a tokens object.
type geminiMessageTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

// geminiMessageWithTokens represents a Gemini message with token usage data.
// Used for extracting token counts from Gemini transcripts.
type geminiMessageWithTokens struct {
	ID     string               `json:"id"`
	Type   string               `json:"type"`
	Tokens *geminiMessageTokens `json:"tokens,omitempty"`
}
