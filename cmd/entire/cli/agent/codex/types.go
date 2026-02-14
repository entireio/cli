package codex

import "encoding/json"

// notifyPayload is the JSON structure sent to the notify command by Codex CLI.
// Codex sends this on agent-turn-complete events.
type notifyPayload struct {
	Type                 string   `json:"type"`
	ThreadID             string   `json:"thread-id"`
	TurnID               string   `json:"turn-id"`
	Cwd                  string   `json:"cwd"`
	InputMessages        []string `json:"input-messages"`
	LastAssistantMessage string   `json:"last-assistant-message"`
}

// rolloutEvent represents a single event line in a Codex rollout JSONL file.
// Codex stores session transcripts as JSONL where each line is a typed event.
// Known types: session_meta, response_item, turn_context, compacted, event_msg.
type rolloutEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Item    json.RawMessage `json:"item,omitempty"`
}

// rolloutItem represents the item/payload content in a rollout event.
type rolloutItem struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status,omitempty"`
	Name   string `json:"name,omitempty"`
}

// eventMsgPayload represents the payload of an "event_msg" rollout event.
// Codex emits event_msg events for various internal events. The "patch_apply_begin"
// subtype contains a changes map where keys are file paths being modified.
// See codex-rs/exec/src/exec_events.rs: PatchApplyBeginEvent { changes: HashMap<PathBuf, FileChange> }
type eventMsgPayload struct {
	Type    string                     `json:"type"`
	Changes map[string]json.RawMessage `json:"changes,omitempty"`
}

// Codex event item types
const (
	ItemTypeFileChange       = "file_change"
	ItemTypeCommandExecution = "command_execution"
	ItemTypeAgentMessage     = "agent_message"
	ItemTypeFunctionCall     = "function_call"
	ItemTypeLocalShellCall   = "local_shell_call"
)

// Codex event_msg subtypes
const (
	EventMsgTypePatchApplyBegin = "patch_apply_begin"
)
