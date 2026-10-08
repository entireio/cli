package codex

import "encoding/json"

// HooksFile represents the .codex/hooks.json structure.
type HooksFile struct {
	Hooks HookEvents `json:"hooks"`
}

// HookEvents contains the hook configurations by event type.
type HookEvents struct {
	SessionStart     []MatcherGroup `json:"SessionStart,omitempty"`
	SessionEnd       []MatcherGroup `json:"SessionEnd,omitempty"`
	UserPromptSubmit []MatcherGroup `json:"UserPromptSubmit,omitempty"`
	Stop             []MatcherGroup `json:"Stop,omitempty"`
	PreToolUse       []MatcherGroup `json:"PreToolUse,omitempty"`
	PostToolUse      []MatcherGroup `json:"PostToolUse,omitempty"`
	SubagentStart    []MatcherGroup `json:"SubagentStart,omitempty"`
	SubagentStop     []MatcherGroup `json:"SubagentStop,omitempty"`
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

// sessionInfoRaw is the JSON structure shared by the session-scoped hooks,
// SessionStart and SessionEnd, which differ only in the event they represent.
//
// SessionEnd's payload is a strict subset: it carries no model or
// permission_mode (it fires after teardown, not within a turn), and swaps
// `source` for a `reason` that is the constant "other" in Codex today, so it
// cannot distinguish quit from /clear. Unmarshalling is not strict, so the
// absent fields simply stay zero.
type sessionInfoRaw struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath *string `json:"transcript_path"` // nullable (ephemeral mode)
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	Source         string  `json:"source"` // SessionStart: "startup", "resume", "clear"
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

// subagentStartRaw is Codex's SubagentStart payload
// (codex-rs/hooks/schema/generated/subagent-start.command.input.schema.json).
//
// session_id is the identity shared by the root thread and all descendants — the
// user's session — while agent_id is this subagent thread's own id.
type subagentStartRaw struct {
	SessionID      string  `json:"session_id"`
	AgentID        string  `json:"agent_id"`
	AgentType      string  `json:"agent_type"`
	TranscriptPath *string `json:"transcript_path"` // nullable; parent rollout
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	TurnID         string  `json:"turn_id"`
}

// subagentStopRaw is Codex's SubagentStop payload
// (codex-rs/hooks/schema/generated/subagent-stop.command.input.schema.json).
//
// Note the two transcripts: transcript_path is the parent thread's rollout,
// agent_transcript_path is the subagent's own.
type subagentStopRaw struct {
	SessionID            string  `json:"session_id"`
	AgentID              string  `json:"agent_id"`
	AgentType            string  `json:"agent_type"`
	TranscriptPath       *string `json:"transcript_path"`       // nullable; parent rollout
	AgentTranscriptPath  *string `json:"agent_transcript_path"` // nullable; subagent rollout
	LastAssistantMessage *string `json:"last_assistant_message"`
	CWD                  string  `json:"cwd"`
	HookEventName        string  `json:"hook_event_name"`
	Model                string  `json:"model"`
	PermissionMode       string  `json:"permission_mode"`
	TurnID               string  `json:"turn_id"`
}
