package codex

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

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
	// extra keeps the fields Entire does not model, so rewriting hooks.json
	// leaves the rest of the user's group as they wrote it.
	extra map[string]json.RawMessage
}

// matcherGroupFields is MatcherGroup without its JSON methods.
type matcherGroupFields MatcherGroup

func (g *MatcherGroup) UnmarshalJSON(data []byte) error {
	*g = MatcherGroup{} // see HookEntry.UnmarshalJSON
	extra, _, err := jsonutil.UnmarshalKeepingExtra(data, (*matcherGroupFields)(g), "matcher", "hooks")
	g.extra = extra
	return err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
}

func (g MatcherGroup) MarshalJSON() ([]byte, error) {
	return jsonutil.MarshalWithExtra(matcherGroupFields(g), g.extra) //nolint:wrapcheck // returned from MarshalJSON; encoding/json adds context
}

// HookEntry represents a single hook handler in the config.
//
// Command is what Codex runs on macOS and Linux. CommandWindows replaces it on
// native Windows: Codex picks between the two by the platform it was built for
// (codex-rs/hooks/src/engine/discovery.rs), so a hooks.json carrying both runs
// the right one wherever it is checked out, whichever shell wrote it.
type HookEntry struct {
	Type           string `json:"type"`
	Command        string `json:"command,omitempty"`
	CommandWindows string `json:"commandWindows,omitempty"`
	Timeout        int    `json:"timeout,omitempty"`
	// extra keeps every field Entire does not model (statusMessage, async,
	// additionalContextLimit, a command_windows spelling, mcp_tool's server and
	// tool, and anything Codex adds later), so a user's own hook survives
	// `entire enable` unchanged.
	extra map[string]json.RawMessage
	// raw holds the entry as written when it sets a modeled field to its zero
	// value, which would otherwise read and re-encode as the field being absent:
	// Codex clamps a timeout of 0 to 1 second but treats an absent one as its
	// 600-second default, and skips a command hook whose command is "" but
	// refuses to load one with no command at all.
	raw json.RawMessage
}

// hookEntryFields is HookEntry without its JSON methods.
type hookEntryFields HookEntry

func (e *HookEntry) UnmarshalJSON(data []byte) error {
	// encoding/json reuses slice elements without zeroing them, so fields kept
	// from a previous decode must not survive into this one.
	*e = HookEntry{}
	extra, present, err := jsonutil.UnmarshalKeepingExtra(data, (*hookEntryFields)(e), "type", "command", "commandWindows", "timeout")
	e.extra = extra
	if err == nil && setsZeroValue(e, present) {
		e.raw = slices.Clone(data)
	}
	return err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
}

// withManagedSettings returns e running want's commands with want's type and
// timeout, keeping every other field e carries. command_windows is dropped:
// Codex rejects a handler that sets both spellings, and commandWindows is
// written.
func (e HookEntry) withManagedSettings(want HookEntry) HookEntry {
	e.Type, e.Command, e.CommandWindows, e.Timeout = want.Type, want.Command, want.CommandWindows, want.Timeout
	e.raw = nil
	if _, ok := e.extra["command_windows"]; ok {
		e.extra = maps.Clone(e.extra)
		delete(e.extra, "command_windows")
	}
	return e
}

// setsZeroValue reports whether one of the present modeled keys of e decoded to
// its zero value, which omitempty would drop on the way back out.
func setsZeroValue(e *HookEntry, present []string) bool {
	for _, key := range present {
		switch key {
		case "type":
			if e.Type == "" {
				return true
			}
		case "command":
			if e.Command == "" {
				return true
			}
		case "commandWindows":
			if e.CommandWindows == "" {
				return true
			}
		case "timeout":
			if e.Timeout == 0 {
				return true
			}
		}
	}
	return false
}

func (e HookEntry) MarshalJSON() ([]byte, error) {
	if e.raw != nil {
		return e.raw, nil
	}
	return jsonutil.MarshalWithExtra(hookEntryFields(e), e.extra) //nolint:wrapcheck // returned from MarshalJSON; encoding/json adds context
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
