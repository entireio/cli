package codex

import "encoding/json"

// HooksFile represents the .codex/hooks.json structure.
type HooksFile struct {
	Hooks HookEvents `json:"hooks"`
}

// HookEvents contains the hook configurations by event type.
type HookEvents struct {
	SessionStart     []MatcherGroup `json:"SessionStart,omitempty"`
	UserPromptSubmit []MatcherGroup `json:"UserPromptSubmit,omitempty"`
	Stop             []MatcherGroup `json:"Stop,omitempty"`
	PreToolUse       []MatcherGroup `json:"PreToolUse,omitempty"`
	PostToolUse      []MatcherGroup `json:"PostToolUse,omitempty"`
}

// MatcherGroup groups hooks under an optional matcher pattern.
type MatcherGroup struct {
	Matcher *string     `json:"matcher"`
	Hooks   []HookEntry `json:"hooks"`
}

// HookEntry represents a single hook command in the config.
type HookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// sessionStartRaw is the JSON structure from SessionStart hooks.
type sessionStartRaw struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath *string `json:"transcript_path"` // nullable
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	Source         string  `json:"source"` // "startup", "resume", "clear"
}

// userPromptSubmitRaw is the JSON structure from UserPromptSubmit hooks.
type userPromptSubmitRaw struct {
	SessionID      string  `json:"session_id"`
	TurnID         string  `json:"turn_id"`
	TranscriptPath *string `json:"transcript_path"` // nullable
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	Prompt         string  `json:"prompt"`
}

// postToolUseRaw is the JSON structure from PostToolUse hooks.
// Schema source: codex-rs/hooks/src/schema.rs PostToolUseCommandInput.
// We only consume the fields we need; unknown fields are ignored.
type postToolUseRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath *string         `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	Model          string          `json:"model"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// applyPatchToolInput is the tool_input shape for apply_patch.
// Codex serializes the patch envelope as a single string under "command".
type applyPatchToolInput struct {
	Command string `json:"command"`
}

// stopRaw is the JSON structure from Stop hooks.
type stopRaw struct {
	SessionID            string  `json:"session_id"`
	TurnID               string  `json:"turn_id"`
	TranscriptPath       *string `json:"transcript_path"` // nullable
	CWD                  string  `json:"cwd"`
	HookEventName        string  `json:"hook_event_name"`
	Model                string  `json:"model"`
	PermissionMode       string  `json:"permission_mode"`
	StopHookActive       bool    `json:"stop_hook_active"`
	LastAssistantMessage *string `json:"last_assistant_message"` // nullable
}

// derefString safely dereferences a nullable string pointer.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
