package agent

import (
	"strings"
	"time"
)

// HookType represents agent lifecycle events
type HookType string

const (
	HookSessionStart     HookType = "session_start"
	HookSessionEnd       HookType = "session_end"
	HookUserPromptSubmit HookType = "user_prompt_submit"
	HookStop             HookType = "stop"
	HookPreToolUse       HookType = "pre_tool_use"
	HookPostToolUse      HookType = "post_tool_use"
)

// HookInput contains normalized data from hook callbacks
type HookInput struct {
	HookType  HookType
	SessionID string
	// SessionRef is an agent-specific session reference (file path, db key, etc.)
	SessionRef string
	Timestamp  time.Time

	// UserPrompt is the user's prompt text (from UserPromptSubmit hooks)
	UserPrompt string

	// Tool-specific fields (PreToolUse/PostToolUse)
	ToolName     string
	ToolUseID    string
	ToolInput    []byte // Raw JSON
	ToolResponse []byte // Raw JSON (PostToolUse only)

	// RawData preserves agent-specific data for extension
	RawData map[string]interface{}
}

// SessionChange represents detected session activity (for FileWatcher)
type SessionChange struct {
	SessionID  string
	SessionRef string
	EventType  HookType
	Timestamp  time.Time
}

// TokenUsage represents aggregated token usage for a checkpoint.
// This is agent-agnostic and can be populated by any agent that tracks token usage.
type TokenUsage struct {
	// InputTokens is the number of input tokens (fresh, not from cache)
	InputTokens int `json:"input_tokens"`
	// CacheCreationTokens is tokens written to cache (billable at cache write rate)
	CacheCreationTokens int `json:"cache_creation_tokens"`
	// CacheReadTokens is tokens read from cache (discounted rate)
	CacheReadTokens int `json:"cache_read_tokens"`
	// OutputTokens is the number of output tokens generated
	OutputTokens int `json:"output_tokens"`
	// APICallCount is the number of API calls made
	APICallCount int `json:"api_call_count"`
	// SubagentTokens contains token usage from spawned subagents (if any)
	SubagentTokens *TokenUsage `json:"subagent_tokens,omitempty"`
}

// FileEditAction represents the type of file edit operation.
type FileEditAction string

const (
	// FileEditActionWrite represents a file write operation (creating or overwriting a file).
	FileEditActionWrite FileEditAction = "write"
	// FileEditActionEdit represents a file edit operation (modifying part of an existing file).
	FileEditActionEdit FileEditAction = "edit"
)

// FileEdit represents a single file edit operation performed by an agent tool.
type FileEdit struct {
	// FilePath is the edited file path, repo-relative with forward slashes (e.g., "cmd/main.go").
	FilePath string `json:"file_path"`
	// Action is the normalized edit action derived from ToolName.
	Action FileEditAction `json:"action"`
	// ToolName is the raw agent tool name (e.g., "Write", "Edit") for provenance.
	ToolName     string    `json:"tool_name"`
	LinesAdded   int       `json:"lines_added"`
	LinesRemoved int       `json:"lines_removed"`
	Timestamp    time.Time `json:"timestamp"`
}

// CountLines counts the number of lines in a string.
// Returns 0 for empty strings, otherwise counts newline-separated lines.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
