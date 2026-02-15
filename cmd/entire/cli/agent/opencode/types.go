package opencode

import "encoding/json"

// Hook names - these become subcommands under `entire hooks opencode`
const (
	HookNameSessionStart = "session-start"
	HookNameStop         = "stop"
	HookNameTaskStart    = "task-start"
	HookNameTaskComplete = "task-complete"
)

// hookInputRaw is the JSON structure from all OpenCode hooks.
// OpenCode sends the same shape for all hook events via the plugin system.
type hookInputRaw struct {
	SessionID              string          `json:"session_id"`
	SessionRef             string          `json:"session_ref"`
	Timestamp              string          `json:"timestamp"`
	ToolName               string          `json:"tool_name,omitempty"`
	ToolUseID              string          `json:"tool_use_id,omitempty"`
	ToolInput              json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse           json.RawMessage `json:"tool_response,omitempty"`
	TranscriptPath         string          `json:"transcript_path"`
	SubagentTranscriptPath string          `json:"subagent_transcript_path,omitempty"`
}

// Tool names used in OpenCode that modify files
const (
	ToolWrite = "file_write"
	ToolEdit  = "file_edit"
	ToolPatch = "file_patch"
)

// FileModificationTools lists tools that create or modify files in OpenCode
var FileModificationTools = []string{
	ToolWrite,
	ToolEdit,
	ToolPatch,
}

// Transcript entry types
const (
	MessageRoleUser      = "user"
	MessageRoleAssistant = "assistant"
)

// Part types in OpenCode transcripts
const (
	PartTypeText       = "text"
	PartTypeTool       = "tool"
	PartTypeStepStart  = "step-start"
	PartTypeStepFinish = "step-finish"
	PartTypePatch      = "patch"
)

// TranscriptEntry represents a single JSONL line in an OpenCode transcript.
type TranscriptEntry struct {
	Info  TranscriptEntryInfo `json:"info"`
	Parts []TranscriptPart    `json:"parts"`
}

// TranscriptEntryInfo contains metadata for a transcript entry.
// Assistant messages include token usage and cost from the provider API.
type TranscriptEntryInfo struct {
	ID        string              `json:"id"`
	SessionID string              `json:"sessionID"`
	Role      string              `json:"role"`
	Time      TranscriptEntryTime `json:"time"`
	Summary   *TranscriptSummary  `json:"summary,omitempty"`
	Tokens    *TranscriptTokens   `json:"tokens,omitempty"`
	Cost      float64             `json:"cost,omitempty"`
}

// TranscriptEntryTime contains timing information.
type TranscriptEntryTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
}

// TranscriptSummary contains a summary of changes in the entry.
type TranscriptSummary struct {
	Title string           `json:"title"`
	Diffs []TranscriptDiff `json:"diffs,omitempty"`
}

// TranscriptDiff represents a file change.
type TranscriptDiff struct {
	File      string `json:"file"`
	Before    string `json:"before"`
	After     string `json:"after"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// TranscriptPart represents a part within a transcript entry.
type TranscriptPart struct {
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type"`
	Text     string               `json:"text,omitempty"`
	Tool     string               `json:"tool,omitempty"`
	CallID   string               `json:"callID,omitempty"`
	FilePath string               `json:"filePath,omitempty"`
	State    *TranscriptToolState `json:"state,omitempty"`
	Tokens   *TranscriptTokens    `json:"tokens,omitempty"`
	Cost     float64              `json:"cost,omitempty"`
	Reason   string               `json:"reason,omitempty"`
}

// TranscriptTokens contains token usage from the provider API.
// Present on assistant message info and step-finish parts.
type TranscriptTokens struct {
	Input     int                   `json:"input"`
	Output    int                   `json:"output"`
	Reasoning int                   `json:"reasoning"`
	Cache     TranscriptTokensCache `json:"cache"`
}

// TranscriptTokensCache contains cache-specific token counts.
type TranscriptTokensCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// TranscriptToolState contains tool execution state.
type TranscriptToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Title  string          `json:"title,omitempty"`
}
