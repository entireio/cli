package transcript

import "encoding/json"

// Transcript records are the append-only form of an Entire Transcript Format
// transcript. An agent-specific adapter reads a redacted agent transcript and
// emits records; it never revises a record it has emitted, so records can be
// streamed and stored as plain appends. The fold (Fold, Folder) is
// agent-agnostic: applying records in order produces the lines Convert writes.
// Convert sends Claude Code, Cursor and Droid transcripts through an adapter
// and the fold; the other converters still write lines directly. The fold is
// meant to be shared with a backend that folds the same records to show a
// live session. README.md specifies the record wire format.

// Op names what a record does to its target message.
type Op string

const (
	// OpMessage creates a message, or replaces the role, timestamp, source ID
	// and content of a known one and restores it if it was dropped.
	OpMessage Op = "message"
	// OpAppend appends content blocks to a message. The record's timestamp
	// replaces the message's, so an append without one clears it. Content
	// that is not an array adds no blocks, and appending to a message whose
	// content is not an array discards that content.
	OpAppend Op = "append"
	// OpResult sets the result of a tool_use block in a message. Nothing
	// changes when the content is not an array, when one of its blocks is not
	// an object, or when no tool_use block matches.
	OpResult Op = "result"
	// OpUsage sets a message's token counts.
	OpUsage Op = "usage"
	// OpDrop hides a message.
	OpDrop Op = "drop"
)

// Record is one transcript record. Which fields apply depends on Op; Role is
// TypeUser or TypeAssistant.
//
// ID is the fold key. It is unique within a session and stable: the same
// transcript yields the same IDs on every run. SourceID is the agent's own ID
// for the message and is what the compact line prints as "id"; it is empty
// when the agent has none. The two differ when the adapter had to derive a key,
// for example when the agent reuses a message ID for a second, non-adjacent
// message that compaction keeps separate.
//
// Zero-length TS and Content mean absent, as they do once the record is
// encoded. JSON null or [] content is present.
//
// When decoding a stream, decode each record into a zero Record: a field the
// next record omits keeps its old value otherwise, so an append without "ts"
// would inherit the previous record's timestamp instead of clearing it.
type Record struct {
	Op       Op     `json:"op"`
	ID       string `json:"id"`
	SourceID string `json:"sourceId,omitempty"`
	Role     string `json:"role,omitempty"`

	// TS is the agent's timestamp, passed through verbatim.
	TS json.RawMessage `json:"ts,omitempty"`
	// Content is a JSON array of compact content blocks. Adapters may pass a
	// non-array value through on OpMessage when the agent wrote one.
	Content json.RawMessage `json:"content,omitempty"`

	// ToolUseID selects the tool_use block an OpResult targets. Empty selects
	// the last tool_use block in the message.
	ToolUseID string      `json:"toolUseId,omitempty"`
	Result    *ToolResult `json:"result,omitempty"`

	// Token counts. A non-zero value replaces the message's previous value.
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
}
