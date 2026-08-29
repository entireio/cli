// Package transcript provides shared types and utilities for parsing JSONL transcripts.
// Used by agents that share the same JSONL format (Claude Code, Cursor).
package transcript

import "encoding/json"

// Message type constants for transcript lines.
const (
	TypeUser      = "user"
	TypeAssistant = "assistant"
)

// Content type constants for content blocks within messages.
const (
	ContentTypeText    = "text"
	ContentTypeToolUse = "tool_use"
)

// Line represents a single line in a Claude Code or Cursor JSONL transcript.
// Claude Code uses "type" to distinguish user/assistant messages.
// Cursor uses "role" for the same purpose.
type Line struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	UUID    string          `json:"uuid"`
	Message json.RawMessage `json:"message"`
}

// UserMessage represents a user message in the transcript.
type UserMessage struct {
	Content interface{} `json:"content"`
}

// AssistantMessage represents an assistant message in the transcript.
type AssistantMessage struct {
	Content []ContentBlock `json:"content"`
}

// ContentBlock represents a block within an assistant message.
type ContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolInput represents the input to various tools.
// Used to extract file paths and descriptions from tool calls.
//
// It is a union of the input keys the supported agents emit: Claude Code's
// snake_case names plus the camelCase / short variants Cursor (filePath),
// OpenCode and Pi (path) use for the same thing. Decode a tool_use input into
// it best-effort — a non-string value under one of these keys is skipped by
// encoding/json rather than aborting the decode, so the remaining fields still
// populate.
type ToolInput struct {
	FilePath     string `json:"file_path,omitempty"`
	NotebookPath string `json:"notebook_path,omitempty"`
	Description  string `json:"description,omitempty"`
	Command      string `json:"command,omitempty"`
	Pattern      string `json:"pattern,omitempty"`
	// FilePathCamel is Cursor's spelling of FilePath.
	FilePathCamel string `json:"filePath,omitempty"`
	// Path is the OpenCode / Pi spelling of FilePath.
	Path string `json:"path,omitempty"`
	// Skill tool fields
	Skill string `json:"skill,omitempty"`
	// Task/Agent tool fields: the dispatched subagent's name and, when the
	// caller pinned one, the model requested for it.
	SubagentType string `json:"subagent_type,omitempty"`
	Model        string `json:"model,omitempty"`
	// WebFetch tool fields
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// AnyFilePath returns the file path under whichever key the agent used —
// the first non-empty of FilePath (file_path), FilePathCamel (filePath) and
// Path (path) — or "" when none is set.
func (in ToolInput) AnyFilePath() string {
	switch {
	case in.FilePath != "":
		return in.FilePath
	case in.FilePathCamel != "":
		return in.FilePathCamel
	default:
		return in.Path
	}
}

// RawDetail returns the first non-empty of Description, Command, FilePath,
// FilePathCamel, Path and Pattern, verbatim. This is the string condensation
// stores in checkpoints as the tool's detail, so its precedence is a stored
// data contract: change it and previously written checkpoints stop matching
// freshly condensed ones.
func (in ToolInput) RawDetail() string {
	for _, v := range []string{in.Description, in.Command, in.FilePath, in.FilePathCamel, in.Path, in.Pattern} {
		if v != "" {
			return v
		}
	}
	return ""
}
