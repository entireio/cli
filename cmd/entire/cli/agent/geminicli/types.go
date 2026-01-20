package geminicli

import "encoding/json"

// GeminiSettings represents the .gemini/settings.json structure
type GeminiSettings struct {
	Tools GeminiToolsConfig `json:"tools,omitempty"`
	Hooks GeminiHooks       `json:"hooks,omitempty"`
}

// GeminiToolsConfig contains tool-related settings
type GeminiToolsConfig struct {
	EnableHooks bool `json:"enableHooks,omitempty"`
}

// GeminiHooks contains all hook configurations
type GeminiHooks struct {
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

// toolHookInputRaw is the JSON structure from BeforeTool/AfterTool hooks
type toolHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	Timestamp      string          `json:"timestamp"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"` // Only for AfterTool
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
