package cursor

import "encoding/json"

// Cursor hooks.json format uses camelCase keys.
// See: .cursor/hooks.json - {"version":1,"hooks":{"sessionStart":[{"command":"..."}],...}}

// CursorHooksFile represents the root structure of .cursor/hooks.json
type CursorHooksFile struct {
	Version int         `json:"version"`
	Hooks   CursorHooks `json:"hooks"`
}

// CursorHooks contains hook arrays keyed by camelCase event names
type CursorHooks struct {
	SessionStart       []CursorHookEntry `json:"sessionStart,omitempty"`
	SessionEnd         []CursorHookEntry `json:"sessionEnd,omitempty"`
	BeforeSubmitPrompt []CursorHookEntry `json:"beforeSubmitPrompt,omitempty"`
	Stop               []CursorHookEntry `json:"stop,omitempty"`
	PreToolUse         []CursorHookEntry `json:"preToolUse,omitempty"`
	PostToolUse        []CursorHookEntry `json:"postToolUse,omitempty"`
}

// CursorHookEntry represents a single hook command in Cursor's format
type CursorHookEntry struct {
	Command string `json:"command"`
}

// Raw payload types for hook stdin (Cursor-specific field names).
// Cursor may send conversation_id, transcript_path, etc.; we use minimal structs
// that can be extended when discovery provides full schema.

// sessionInfoRaw is the JSON structure from SessionStart/SessionEnd/Stop hooks
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	TranscriptPath string `json:"transcript_path"`
}

// userPromptSubmitRaw is the JSON structure from beforeSubmitPrompt hooks
type userPromptSubmitRaw struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

// taskHookInputRaw is the JSON structure from preToolUse[Task] / postToolUse
type taskHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	ConversationID string          `json:"conversation_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// postToolHookInputRaw is the JSON structure from postToolUse hooks
type postToolHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	ConversationID string          `json:"conversation_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   struct {
		AgentID string `json:"agentId"`
	} `json:"tool_response"`
}
