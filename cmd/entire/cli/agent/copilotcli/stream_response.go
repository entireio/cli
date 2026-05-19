package copilotcli

import "time"

// copilotStreamEvent is one decoded line of `copilot --output-format json
// --stream on` NDJSON output. Fields are populated based on Type.
//
// Real-CLI shape (GitHub Copilot CLI 1.0.48):
//   - session.mcp_servers_loaded / session.skills_loaded / session.tools_updated
//     → session bootstrap, ignored
//   - user.message                                       → echoed input, ignored
//   - assistant.turn_start                               → turn begins
//   - assistant.message_start (ephemeral)                → announces messageId
//   - assistant.message_delta (ephemeral, deltaContent)  → incremental tokens
//   - assistant.message (non-ephemeral, outputTokens)    → consolidated message
//     with final outputTokens count
//   - assistant.turn_end                                 → turn marker
//   - result (terminal, usage.totalApiDurationMs etc.)   → exitCode + duration
type copilotStreamEvent struct {
	Type      string             `json:"type"`
	Timestamp time.Time          `json:"timestamp,omitempty"`
	Data      *copilotStreamData `json:"data,omitempty"`
	ID        string             `json:"id,omitempty"`
	ParentID  string             `json:"parentId,omitempty"`
	Ephemeral bool               `json:"ephemeral,omitempty"`

	// SessionID is populated on the terminal type=result event.
	SessionID string `json:"sessionId,omitempty"`

	// ExitCode is populated on the terminal type=result event. 0=success.
	ExitCode int `json:"exitCode,omitempty"`

	// Usage is populated on the terminal type=result event.
	Usage *copilotStreamUsage `json:"usage,omitempty"`
}

// copilotStreamData is the per-event payload. Fields are sparsely populated
// based on the parent event's Type.
type copilotStreamData struct {
	MessageID    string `json:"messageId,omitempty"`
	TurnID       string `json:"turnId,omitempty"`
	DeltaContent string `json:"deltaContent,omitempty"`
	Model        string `json:"model,omitempty"`
	Content      string `json:"content,omitempty"`

	// OutputTokens appears on the consolidated assistant.message event.
	// Copilot does not expose input/cached tokens in its public stream.
	OutputTokens int `json:"outputTokens,omitempty"`
}

// copilotStreamUsage appears inside the terminal type=result event. Copilot
// reports timing data and "premium request" counts but does not surface input
// or cached-input token counts in its public stream.
type copilotStreamUsage struct {
	PremiumRequests    int `json:"premiumRequests,omitempty"`
	TotalAPIDurationMs int `json:"totalApiDurationMs,omitempty"`
	SessionDurationMs  int `json:"sessionDurationMs,omitempty"`
}
