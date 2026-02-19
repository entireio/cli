package opencode

// sessionInfoRaw matches the JSON payload piped from the OpenCode plugin for session events.
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// turnStartRaw matches the JSON payload for turn-start (user prompt submission).
type turnStartRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

// --- Transcript types (JSONL format — one Message per line) ---

// Message role constants.
const (
	roleAssistant = "assistant"
	roleUser      = "user"
)

// Message represents a single message (one line) in the JSONL transcript.
type Message struct {
	ID      string  `json:"id"`
	Role    string  `json:"role"` // "user" or "assistant"
	Content string  `json:"content"`
	Time    Time    `json:"time"`
	Tokens  *Tokens `json:"tokens,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
	Parts   []Part  `json:"parts,omitempty"`
}

// Time holds message timestamps.
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
}

// Cache holds cache-related token counts.
type Cache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// Part represents a message part (text, tool, etc.).
type Part struct {
	Type   string     `json:"type"` // "text", "tool", etc.
	Text   string     `json:"text,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	CallID string     `json:"callID,omitempty"`
	State  *ToolState `json:"state,omitempty"`
}

// ToolState represents tool execution state.
type ToolState struct {
	Status string                 `json:"status"` // "pending", "running", "completed", "error"
	Input  map[string]interface{} `json:"input,omitempty"`
	Output string                 `json:"output,omitempty"`
}

// FileModificationTools are tools in OpenCode that modify files on disk.
// These match the actual tool names from OpenCode's source:
//   - edit:  internal/llm/tools/edit.go  (EditToolName)
//   - write: internal/llm/tools/write.go (WriteToolName)
//   - patch: internal/llm/tools/patch.go (PatchToolName)
var FileModificationTools = []string{
	"edit",
	"write",
	"patch",
}
