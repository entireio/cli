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

// FileEditAction represents the type of file edit operation.
type FileEditAction string

const (
	// FileEditActionWrite represents a Write tool operation (create or overwrite).
	FileEditActionWrite FileEditAction = "write"
	// FileEditActionEdit represents an Edit tool operation (modify existing file).
	FileEditActionEdit FileEditAction = "edit"
)

// FileEdit represents a single file modification by an agent tool.
// Stored in append-only JSONL logs per session for real-time file tracking
// and future attribution computation.
type FileEdit struct {
	// FilePath is the repo-relative path to the modified file.
	FilePath string `json:"file_path"`
	// Action is the type of edit (write or edit).
	Action FileEditAction `json:"action"`
	// ToolName is the agent tool that performed the edit (e.g., "Write", "Edit").
	ToolName string `json:"tool_name"`
	// LinesAdded is the number of lines added by this edit.
	LinesAdded int `json:"lines_added"`
	// LinesRemoved is the number of lines removed by this edit.
	LinesRemoved int `json:"lines_removed"`
	// Timestamp is when the edit occurred.
	Timestamp time.Time `json:"timestamp"`
}

// CountLines counts the number of lines in a string.
// Empty string returns 0. A string with no newlines returns 1.
// Trailing newlines are not counted as an additional line.
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
