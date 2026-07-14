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
type ToolInput struct {
	FilePath     string `json:"file_path,omitempty"`
	NotebookPath string `json:"notebook_path,omitempty"`
	Description  string `json:"description,omitempty"`
	Command      string `json:"command,omitempty"`
	Pattern      string `json:"pattern,omitempty"`
	// Skill tool fields
	Skill string `json:"skill,omitempty"`
	// WebFetch tool fields
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}
