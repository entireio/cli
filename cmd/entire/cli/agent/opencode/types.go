package opencode

// sessionInfoRaw matches the JSON payload piped from the OpenCode plugin for session events.
// The plugin sends only session_id; Go calls `opencode export` to get the transcript.
type sessionInfoRaw struct {
	SessionID string `json:"session_id"`
}

// turnStartRaw matches the JSON payload for turn-start (user prompt submission).
type turnStartRaw struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
}

// turnEndRaw matches the JSON payload for turn-end (session idle).
// Extends sessionInfoRaw with model info captured during the turn.
type turnEndRaw struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
}

// --- Export JSON types (from `opencode export`) ---

// ExportSession represents the top-level structure of `opencode export` output.
// This is OpenCode's native format for session data.
type ExportSession struct {
	Info     SessionInfo     `json:"info"`
	Messages []ExportMessage `json:"messages"`
}

// SessionInfo contains session metadata from the export.
type SessionInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

// ExportMessage represents a single message in the export format.
// Each message contains info (metadata) and parts (content).
type ExportMessage struct {
	Info  MessageInfo `json:"info"`
	Parts []Part      `json:"parts"`
}

// MessageInfo contains message metadata.
//
// ModelID and ProviderID are top-level keys of an ASSISTANT message's info
// (e.g. "claude-opus-4-6" / "anthropic"), confirmed against `opencode export`
// output and the OpenCode 1.3 message store (2026-08-28). A user message
// records its model differently — nested as info.model{providerID,modelID} —
// and that shape is not decoded here. Cost is OpenCode's own dollar figure
// for the message, 0 when the provider reports none.
type MessageInfo struct {
	ID         string  `json:"id"`
	SessionID  string  `json:"sessionID,omitempty"`
	Role       string  `json:"role"` // "user" or "assistant"
	Time       Time    `json:"time"`
	Tokens     *Tokens `json:"tokens,omitempty"`
	Cost       float64 `json:"cost,omitempty"`
	ModelID    string  `json:"modelID,omitempty"`
	ProviderID string  `json:"providerID,omitempty"`
}

// Message role constants.
const (
	roleAssistant = "assistant"
	roleUser      = "user"
)

// Time holds message timestamps as Unix epoch MILLISECONDS (e.g.
// 1773867525015), the unit OpenCode writes and transcript/compact converts
// with time.UnixMilli. Completed is 0 while a message is in flight.
type Time struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

// Tokens holds token usage from assistant messages.
type Tokens struct {
	Input     int   `json:"input"`
	Output    int   `json:"output"`
	Reasoning int   `json:"reasoning"`
	Cache     Cache `json:"cache"`
	// Total is OpenCode's own sum. Whether it equals input+output+cache or
	// input+output+reasoning+cache tells us if reasoning is already inside
	// output — see BilledOutput.
	Total int `json:"total"`
}

// BilledOutput returns the output tokens billed at the output rate, counting
// reasoning exactly once. OpenCode has reported reasoning both inside and
// outside output depending on version/provider; both shapes exist in this
// repo's committed history (2026-08-27: 7,456 messages outside, 2,991 inside).
// The total identity disambiguates per message: when total already covers
// reasoning as a separate term, reasoning must be added to output.
func (t Tokens) BilledOutput() int {
	if t.Reasoning > 0 && t.Total == t.Input+t.Output+t.Reasoning+t.Cache.Read+t.Cache.Write {
		return t.Output + t.Reasoning
	}
	return t.Output
}

// Cache holds cache-related token counts.
type Cache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// Part represents a message part (text, tool, etc.).
type Part struct {
	ID     string     `json:"id,omitempty"` // Part ID (e.g., "prt_..."), added in OpenCode 1.2.x
	Type   string     `json:"type"`         // "text", "tool", etc.
	Text   string     `json:"text,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	CallID string     `json:"callID,omitempty"`
	State  *ToolState `json:"state,omitempty"`
}

// ToolState represents tool execution state.
type ToolState struct {
	Status   string             `json:"status"` // "pending", "running", "completed", "error"
	Input    map[string]any     `json:"input,omitempty"`
	Output   string             `json:"output,omitempty"`
	Metadata *ToolStateMetadata `json:"metadata,omitempty"`
}

// ToolStateMetadata holds metadata from tool execution results.
//
// SessionID and Model are written by the task tool (OpenCode 1.3 message
// store, 2026-08-28): SessionID is the child session the subagent ran in —
// a separate session, never part of this export — and Model is the model
// that child actually used. Both are absent on every other tool.
type ToolStateMetadata struct {
	Files     []ToolFileInfo `json:"files,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	Model     *ModelRef      `json:"model,omitempty"`
}

// ModelRef is OpenCode's provider/model pair as nested under a task tool's
// state.metadata.model and a user message's info.model.
type ModelRef struct {
	ModelID    string `json:"modelID,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
}

// ToolFileInfo represents a file affected by a tool operation.
type ToolFileInfo struct {
	FilePath     string `json:"filePath"`
	RelativePath string `json:"relativePath,omitempty"`
}

// FileModificationTools are tools in OpenCode that modify files on disk.
// These match the actual tool names from OpenCode's source (packages/opencode/src/tool/):
//   - edit:        edit.ts  — exact string replacement in existing files
//   - write:       write.ts — create or overwrite files
//   - apply_patch: apply_patch.ts — unified diff patches (used by gpt-* models except gpt-4)
//
// Tool selection is mutually exclusive: apply_patch is enabled for gpt-* (non-gpt-4, non-oss)
// models; edit+write are enabled for all other models (Claude, Gemini, gpt-4, etc.).
// The batch tool (experimental) creates separate transcript parts per sub-call,
// so its children are already captured by this list.
var FileModificationTools = []string{
	"edit",
	"write",
	"apply_patch",
}
