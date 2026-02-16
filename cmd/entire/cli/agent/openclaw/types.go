package openclaw

// OpenClaw session transcript types.
// OpenClaw stores sessions as JSONL files where each line is a JSON object
// representing a message in the conversation.

// Tool names used in OpenClaw transcripts that modify files
const (
	ToolWrite = "write"
	ToolEdit  = "edit"
)

// FileModificationTools lists tools that create or modify files
var FileModificationTools = []string{
	ToolWrite,
	ToolEdit,
}

// OpenClawToolCall represents a tool call in an assistant message
type OpenClawToolCall struct {
	Name   string                 `json:"name"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// OpenClawMessage represents a single JSONL entry in an OpenClaw transcript.
// Each line in the JSONL file is one of:
//   - {"role":"user","content":"...","timestamp":"..."}
//   - {"role":"assistant","content":"...","tool_calls":[...],"timestamp":"..."}
type OpenClawMessage struct {
	Role      string             `json:"role"`
	Content   string             `json:"content,omitempty"`
	ToolCalls []OpenClawToolCall `json:"tool_calls,omitempty"`
	Timestamp string             `json:"timestamp,omitempty"`
}

// SessionMetadata holds OpenClaw session metadata.
// OpenClaw stores session data at ~/.openclaw/sessions/<session-id>/transcript.jsonl
type SessionMetadata struct {
	SessionID string `json:"session_id"`
	RepoPath  string `json:"repo_path,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}
