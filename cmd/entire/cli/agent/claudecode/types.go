package claudecode

import "encoding/json"

// ClaudeSettings represents the .claude/settings.json structure
type ClaudeSettings struct {
	Hooks ClaudeHooks `json:"hooks"`
}

// ClaudeHooks contains the hook configurations
type ClaudeHooks struct {
	SessionStart     []ClaudeHookMatcher `json:"SessionStart,omitempty"`
	SessionEnd       []ClaudeHookMatcher `json:"SessionEnd,omitempty"`
	UserPromptSubmit []ClaudeHookMatcher `json:"UserPromptSubmit,omitempty"`
	Stop             []ClaudeHookMatcher `json:"Stop,omitempty"`
	SubagentStop     []ClaudeHookMatcher `json:"SubagentStop,omitempty"`
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
	// Timeout is the hook's timeout in seconds. Omitted (0) leaves Claude Code's
	// default in place; set only where a hook needs an explicit budget.
	Timeout int `json:"timeout,omitempty"`
}

// sessionInfoRaw is the JSON structure from SessionStart/SessionEnd/Stop hooks.
// SessionStart includes a "model" field with the LLM model identifier.
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model,omitempty"`
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

// subagentStopHookInputRaw is the JSON structure from the SubagentStop hook.
// Per the Agent SDK docs this also carries hook_event_name and cwd, which
// entire has no use for and so doesn't parse. agent_transcript_path is
// parsed defensively: an absent field just leaves AgentTranscriptPath empty
// rather than erroring, and the lifecycle layer then falls back to resolving
// the subagent transcript from AgentID.
type subagentStopHookInputRaw struct {
	SessionID           string `json:"session_id"`
	TranscriptPath      string `json:"transcript_path"`
	AgentID             string `json:"agent_id"`
	AgentTranscriptPath string `json:"agent_transcript_path"`
	ToolUseID           string `json:"tool_use_id"`
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
	// OutputTokensDetails.ThinkingTokens is the reasoning part of OutputTokens
	// (a subset). Present since Claude Code 2.1.x (Aug 2026); absent → 0.
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	// CacheCreation splits CacheCreationInputTokens by TTL; the 1-hour part is
	// priced 2× input (5-minute: 1.25×). Absent on older transcripts → 0.
	CacheCreation struct {
		Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// messageWithUsage represents an assistant message with usage data.
// Used for extracting token counts from Claude Code transcripts.
type messageWithUsage struct {
	ID    string       `json:"id"`
	Model string       `json:"model"`
	Usage messageUsage `json:"usage"`
}

// contentTypeToolResult is the content block type of a tool's output in a
// user row (the assistant-side counterpart is transcript.ContentTypeToolUse).
const contentTypeToolResult = "tool_result"

// contentBlockRaw is one content block of either role: tool_use blocks fill
// ID/Name/Input, tool_result blocks fill ToolUseID/Content. It is the struct
// ExtractSpawnedAgentIDs decodes tool results into as well.
type contentBlockRaw struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}
