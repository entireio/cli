package devin

import "encoding/json"

// HookMatcher matches hooks to tool-name regex patterns.
// Same shape as Claude Code's matcher config — Devin's hooks.v1.json uses the
// Claude Code hook format, where matcher is a regex over tool_name.
type HookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []HookEntry `json:"hooks"`
}

// HookEntry represents a single hook command.
type HookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// sessionInfoRaw is the JSON payload of SessionStart/Stop/SessionEnd hooks.
// Devin payloads carry no transcript_path or cwd (verified live, 3000.2.17);
// the transcript location is derived from session_id instead.
type sessionInfoRaw struct {
	SessionID string `json:"session_id"`
	// Source is how the session started (SessionStart only, e.g. "startup").
	Source string `json:"source,omitempty"`
	// Reason is why the session ended (SessionEnd only, e.g. "session_complete").
	Reason string `json:"reason,omitempty"`
}

// userPromptSubmitRaw is the JSON payload of UserPromptSubmit hooks.
type userPromptSubmitRaw struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	PromptID  string `json:"prompt_id"`
}

// postToolUseRaw is the JSON payload of PostToolUse hooks.
type postToolUseRaw struct {
	SessionID    string          `json:"session_id"`
	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse struct {
		Success bool `json:"success"`
	} `json:"tool_response"`
}

// fileToolInput is the tool_input shape shared by Devin's file-modification
// tools (write, edit, notebook_edit): an absolute file_path plus tool-specific
// fields this integration doesn't need.
type fileToolInput struct {
	FilePath string `json:"file_path"`
}

// Devin tool names that create or modify files. Devin uses lowercase tool
// names (write/edit), unlike Claude Code's Write/Edit.
const (
	ToolWrite        = "write"
	ToolEdit         = "edit"
	ToolNotebookEdit = "notebook_edit"
)

// fileModificationToolsMatcher is the hooks.v1.json regex matcher used to
// subscribe the post-tool-use hook only to file-modifying tools.
const fileModificationToolsMatcher = "^(write|edit|notebook_edit)$"

// isFileModificationTool reports whether a Devin tool name modifies files.
func isFileModificationTool(name string) bool {
	return name == ToolWrite || name == ToolEdit || name == ToolNotebookEdit
}

// --- ATIF transcript format (schema_version ATIF-v1.7) ---

// ATIFTranscript is Devin's native transcript document, written to
// <data-dir>/cli/transcripts/<session_id>.json when a session ends and by
// `devin --export`.
type ATIFTranscript struct {
	SchemaVersion string          `json:"schema_version"`
	SessionID     string          `json:"session_id"`
	Agent         json.RawMessage `json:"agent,omitempty"`
	Steps         []ATIFStep      `json:"steps"`
	FinalMetrics  json.RawMessage `json:"final_metrics,omitempty"`
}

// atifAgentInfo is the subset of the ATIF agent block this integration reads.
type atifAgentInfo struct {
	ModelName string `json:"model_name"`
}

// ATIFStep is a single trajectory step. Extra fields are preserved via Raw
// when chunking so reassembly is lossless.
type ATIFStep = json.RawMessage

// atifStepInfo is the subset of step fields this integration reads.
type atifStepInfo struct {
	Source    string         `json:"source"`
	ModelName string         `json:"model_name,omitempty"`
	ToolCalls []atifToolCall `json:"tool_calls,omitempty"`
	Metrics   *atifMetrics   `json:"metrics,omitempty"`
}

// atifToolCall is a tool invocation recorded on an agent step.
type atifToolCall struct {
	FunctionName string          `json:"function_name"`
	Arguments    json.RawMessage `json:"arguments"`
}

// atifMetrics is per-step token accounting. prompt_tokens includes cache
// reads (verified: cached_tokens ≈ prompt_tokens of the previous step).
type atifMetrics struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}
