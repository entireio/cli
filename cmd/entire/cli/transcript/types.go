// Package transcript provides shared types and utilities for parsing JSONL
// transcripts — the Line/Message/ContentBlock shapes agents with Claude Code's
// JSONL format (Claude Code, Cursor) share — plus the tool-input helpers every
// agent's token attribution uses: ToolInput, ToolInputFromJSON,
// ToolInputFromMap, StringArg and ToolDetail.
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
// OpenCode and Pi (path) use for the same thing. Build one from raw JSON with
// ToolInputFromJSON or from an already-decoded map with ToolInputFromMap; both
// are best-effort — a non-string value under one of these keys leaves that
// field empty while the remaining fields still populate. Note ToolInputFromJSON
// inherits encoding/json's case-insensitive key matching, so it is NOT
// suitable where the exact key spelling matters; compaction uses RawToolDetail
// for that reason. ToolInputFromMap matches keys exactly.
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

// ToolInputFromJSON decodes a tool_use input best-effort: a non-string value
// under a known key makes encoding/json report an UnmarshalTypeError while the
// remaining fields still populate, so the partial result is returned either
// way and the error carries nothing (invalid JSON yields a zero ToolInput).
// Keys fold case as encoding/json does — `Command` fills Command.
func ToolInputFromJSON(raw json.RawMessage) ToolInput {
	var in ToolInput
	_ = json.Unmarshal(raw, &in) //nolint:errcheck // best-effort partial decode, see doc
	return in
}

// ToolInputFromMap reads an already-decoded tool input (OpenCode's
// state.input, Gemini's toolCalls[].args) into a ToolInput without
// re-marshalling it: only the string values under ToolInput's own JSON keys
// are picked — file_path, filePath, path, notebook_path, description,
// command, pattern, skill, subagent_type, model, url, prompt. A non-string
// value under one of them and every other key (a tool's `content`, however
// large, included) are left alone. Keys are matched exactly, so unlike
// ToolInputFromJSON `Command` does not fill Command. A nil map yields a zero
// ToolInput.
func ToolInputFromMap(m map[string]any) ToolInput {
	str := func(key string) string { return StringArg(m, key) }
	return ToolInput{
		FilePath:      str("file_path"),
		NotebookPath:  str("notebook_path"),
		Description:   str("description"),
		Command:       str("command"),
		Pattern:       str("pattern"),
		FilePathCamel: str("filePath"),
		Path:          str("path"),
		Skill:         str("skill"),
		SubagentType:  str("subagent_type"),
		Model:         str("model"),
		URL:           str("url"),
		Prompt:        str("prompt"),
	}
}

// StringArg is the string under key in an already-decoded tool input (a
// tool_use input, OpenCode state.input, Gemini toolCalls[].args); "" when the
// key is absent, the map is nil, or the value is not a string. Keys are
// matched exactly.
func StringArg(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// rawDetailKeys is the order RawToolDetail consults a tool_use input's keys.
// It is a stored data contract (see RawToolDetail): do not reorder or extend.
var rawDetailKeys = []string{"description", "command", "file_path", "filePath", "path", "pattern"}

// RawToolDetail returns the first non-empty JSON string among a tool_use
// input's description, command, file_path, filePath, path and pattern keys,
// verbatim, matching key names EXACTLY (case-sensitive; `Command` and
// `FILE_PATH` do not count). A value under a matching key that is not a
// string (number, object, null) is skipped, and a raw input that is missing,
// empty, or not a JSON object yields "".
//
// This is the string condensation stores in checkpoints as the tool's detail,
// so the precedence and exact-key semantics are a stored data contract:
// change them and previously written checkpoints stop matching freshly
// condensed ones. It deliberately does not go through ToolInput, whose
// encoding/json decoding folds key case.
func RawToolDetail(raw json.RawMessage) string {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	for _, key := range rawDetailKeys {
		var v string
		if r, ok := input[key]; ok && json.Unmarshal(r, &v) == nil && v != "" {
			return v
		}
	}
	return ""
}
