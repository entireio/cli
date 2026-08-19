package openhands

import "encoding/json"

// Event is one entry in an OpenHands conversation.
//
// Events are discriminated by Kind. Only the fields Entire reads are modelled;
// serialization always goes through the raw bytes, so unmodelled fields survive
// a checkpoint round trip untouched.
type Event struct {
	ID         string      `json:"id"`
	Timestamp  string      `json:"timestamp"`
	Source     string      `json:"source"`
	Kind       string      `json:"kind"`
	LLMMessage *LLMMessage `json:"llm_message,omitempty"`
	ToolName   string      `json:"tool_name,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	Thought    []Content   `json:"thought,omitempty"`
}

// LLMMessage is the conversational payload on a MessageEvent.
type LLMMessage struct {
	Role    string    `json:"role"`
	Content []Content `json:"content"`
}

// Content is a single content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolCall is the tool invocation on an ActionEvent.
//
// Arguments is a JSON-encoded *string*, not an object, so reading a file path
// out of it needs a second unmarshal.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// hookPayload mirrors HookEvent in openhands/sdk/hooks/types.py.
//
// There is no transcript_path field, so the conversation directory is
// reconstructed from SessionID.
type hookPayload struct {
	EventType  string          `json:"event_type"`
	SessionID  string          `json:"session_id"`
	WorkingDir string          `json:"working_dir"`
	Message    string          `json:"message"`
	ToolName   string          `json:"tool_name,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// hookMatcher mirrors HookMatcher. The matcher defaults to "*" (all tools).
type hookMatcher struct {
	Matcher string           `json:"matcher"`
	Hooks   []hookDefinition `json:"hooks"`
}

// hookDefinition mirrors HookDefinition.
//
// The JSON key for Async is "async"; OpenHands aliases it to async_ internally
// because async is a reserved word in Python.
type hookDefinition struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}
