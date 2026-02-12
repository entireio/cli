package claudecode

import "encoding/json"

// ClaudeSettings represents the .claude/settings.json structure
type ClaudeSettings struct {
	Hooks ClaudeHooks `json:"hooks"`
}

// rawClaudeHooks preserves unknown hook types using RawMessage
// This prevents silently dropping hooks not defined in ClaudeHooks struct
type rawClaudeHooks map[string]json.RawMessage

// ClaudeHooks contains the hook configurations
type ClaudeHooks struct {
	SessionStart     []ClaudeHookMatcher `json:"SessionStart,omitempty"`
	SessionEnd       []ClaudeHookMatcher `json:"SessionEnd,omitempty"`
	UserPromptSubmit []ClaudeHookMatcher `json:"UserPromptSubmit,omitempty"`
	Stop             []ClaudeHookMatcher `json:"Stop,omitempty"`
	PreToolUse       []ClaudeHookMatcher `json:"PreToolUse,omitempty"`
	PostToolUse      []ClaudeHookMatcher `json:"PostToolUse,omitempty"`
}

// ClaudeHookMatcher matches hooks to specific patterns
type ClaudeHookMatcher struct {
	Matcher string            `json:"matcher"`
	Hooks   []ClaudeHookEntry `json:"hooks"`
}

// ClaudeHookEntry represents a single hook command
type ClaudeHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// sessionInfoRaw is the JSON structure from SessionStart/SessionEnd/Stop hooks
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// userPromptSubmitRaw is the JSON structure from UserPromptSubmit hooks.
// Unlike other session hooks, this includes the user's prompt text.
type userPromptSubmitRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

// taskHookInputRaw is the JSON structure from PreToolUse[Task] hook
type taskHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// postToolHookInputRaw is the JSON structure from PostToolUse hooks
type postToolHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   struct {
		AgentID string `json:"agentId"`
	} `json:"tool_response"`
}

// Tool names used in Claude Code transcripts
const (
	ToolWrite        = "Write"
	ToolEdit         = "Edit"
	ToolNotebookEdit = "NotebookEdit"
	ToolMCPWrite     = "mcp__acp__Write" //nolint:gosec // G101: This is a tool name, not a credential
	ToolMCPEdit      = "mcp__acp__Edit"
)

// FileModificationTools lists tools that create or modify files
var FileModificationTools = []string{
	ToolWrite,
	ToolEdit,
	ToolNotebookEdit,
	ToolMCPWrite,
	ToolMCPEdit,
}

// messageUsage represents token usage from a Claude API response.
// This is specific to Claude/Anthropic's API format.
type messageUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// messageWithUsage represents an assistant message with usage data.
// Used for extracting token counts from Claude Code transcripts.
type messageWithUsage struct {
	ID    string       `json:"id"`
	Usage messageUsage `json:"usage"`
}
