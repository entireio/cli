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
// Codex stores session transcripts as JSONL where each line is an event.
type rolloutEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id,omitempty"`
	TurnID   string          `json:"turn_id,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
	Text     string          `json:"text,omitempty"`
}

// rolloutItem represents the item field in a rollout event.
type rolloutItem struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status,omitempty"`
}

// Codex event item types
const (
	ItemTypeFileChange       = "file_change"
	ItemTypeCommandExecution = "command_execution"
	ItemTypeAgentMessage     = "agent_message"
)
