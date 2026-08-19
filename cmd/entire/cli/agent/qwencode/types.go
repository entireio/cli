package qwencode

import "encoding/json"

// Line is one JSONL entry in a Qwen Code session file.
//
// The envelope is Claude-shaped (uuid/parentUuid/sessionId/type) while the
// nested message is Gemini-shaped (role "model", parts[], usageMetadata). See
// AGENT.md — the split matters because neither existing parser fits on its own.
type Line struct {
	UUID       string     `json:"uuid"`
	ParentUUID string     `json:"parentUuid"`
	SessionID  string     `json:"sessionId"`
	Timestamp  string     `json:"timestamp"`
	Type       string     `json:"type"`
	Provenance string     `json:"provenance"`
	CWD        string     `json:"cwd"`
	Version    string     `json:"version"`
	Model      string     `json:"model,omitempty"`
	Message    *Message   `json:"message,omitempty"`
	Usage      *UsageMeta `json:"usageMetadata,omitempty"`
}

// Message is the Gemini-style payload.
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

// Part is one content block. Exactly one field is populated per part.
type Part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

// FunctionCall is a tool invocation.
type FunctionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// FunctionResponse is a tool result.
type FunctionResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// UsageMeta is Gemini's token accounting block, carried per assistant message.
type UsageMeta struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

// hookPayload is the JSON Qwen writes to a hook's stdin.
//
// TranscriptPath is why this integration needs no export command: Qwen names the
// session file directly, so Entire never reconstructs the on-disk path.
type hookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Prompt         string `json:"prompt,omitempty"`
}

// hookMatcherEntry groups commands behind an optional matcher.
type hookMatcherEntry struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

// hookCommand is a single command. Qwen documents four executor types; Entire
// only ever writes "command".
type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}
