package factoryaidroid

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// FactorySettings represents the .factory/settings.json structure.
type FactorySettings struct {
	Hooks FactoryHooks `json:"hooks"`
}

// FactoryHooks contains the hook configurations.
type FactoryHooks struct {
	SessionStart     []FactoryHookMatcher `json:"SessionStart,omitempty"`
	SessionEnd       []FactoryHookMatcher `json:"SessionEnd,omitempty"`
	UserPromptSubmit []FactoryHookMatcher `json:"UserPromptSubmit,omitempty"`
	Stop             []FactoryHookMatcher `json:"Stop,omitempty"`
	PreToolUse       []FactoryHookMatcher `json:"PreToolUse,omitempty"`
	PostToolUse      []FactoryHookMatcher `json:"PostToolUse,omitempty"`
	PreCompact       []FactoryHookMatcher `json:"PreCompact,omitempty"`
}

// FactoryHookMatcher matches hooks to specific patterns.
type FactoryHookMatcher struct {
	Matcher string             `json:"matcher"`
	Hooks   []FactoryHookEntry `json:"hooks"`
}

// FactoryHookEntry represents a single hook command.
type FactoryHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// sessionInfoRaw is the JSON structure from SessionStart/SessionEnd/Stop/SubagentStop/PreCompact hooks.
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// userPromptSubmitRaw is the JSON structure from UserPromptSubmit hooks.
type userPromptSubmitRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
}

// stopRaw is the JSON structure from Stop hooks.
// Extends sessionInfoRaw with model info captured during the turn.
type stopRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model"`
}

// taskHookInputRaw is the JSON structure from PreToolUse[Task] hook.
type taskHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// postToolHookInputRaw is the JSON structure from PostToolUse[Task] hook.
type postToolHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

func fallbackToolUseID(sessionID, toolName string, toolInput json.RawMessage) string {
	sum := sha256.Sum256([]byte(sessionID + "\n" + toolName + "\n" + string(toolInput)))
	return "factorytask_" + hex.EncodeToString(sum[:8])
}

func fallbackToolFingerprint(toolName string, toolInput json.RawMessage) string {
	sum := sha256.Sum256([]byte(toolName + "\n" + string(toolInput)))
	return hex.EncodeToString(sum[:16])
}

// Tool names used in Factory Droid transcripts.
const (
	ToolCreate       = "Create"
	ToolWrite        = "Write"
	ToolEdit         = "Edit"
	ToolMultiEdit    = "MultiEdit"
	ToolNotebookEdit = "NotebookEdit"
)

// FileModificationTools lists tools that create or modify files.
var FileModificationTools = []string{
	ToolCreate,
	ToolWrite,
	ToolEdit,
	ToolMultiEdit,
	ToolNotebookEdit,
}

// messageUsage represents token usage from an API response.
type messageUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// messageWithUsage represents an assistant message with usage data.
type messageWithUsage struct {
	ID    string       `json:"id"`
	Usage messageUsage `json:"usage"`
}
