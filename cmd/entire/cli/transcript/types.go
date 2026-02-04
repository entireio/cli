// Package transcript provides shared types for parsing Claude Code transcripts.
// This package contains only data structures and constants, not parsing logic.
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

// Line represents a single line in a Claude Code JSONL transcript.
type Line struct {
	Type    string          `json:"type"`
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
