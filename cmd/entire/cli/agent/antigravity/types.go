package antigravity

import "encoding/json"

// HooksFile maps hook names to their event configurations.
// The top-level key is the hook name (user-defined, e.g. "my-linter-hook").
type HooksFile = map[string]HookConfig

// HookConfig defines the event handlers for a named hook entry. It mirrors
// agy's hooks.json schema (https://antigravity.google/docs — Hooks), including
// event keys we don't install (PostToolUse/PostInvocation) so round-tripping a
// user's file never drops data and the install idempotency comparison detects
// stale Entire entries that still carry them.
type HookConfig struct {
	Enabled        *bool           `json:"enabled,omitempty"`
	PreToolUse     []ToolHandler   `json:"PreToolUse,omitempty"`
	PostToolUse    []ToolHandler   `json:"PostToolUse,omitempty"`
	PreInvocation  []SimpleHandler `json:"PreInvocation,omitempty"`
	PostInvocation []SimpleHandler `json:"PostInvocation,omitempty"`
	Stop           []SimpleHandler `json:"Stop,omitempty"`
}

// ToolHandler is a matcher + handlers entry used for PreToolUse / PostToolUse.
type ToolHandler struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks,omitempty"`
}

// SimpleHandler is a direct handler entry used for PreInvocation, PostInvocation, and Stop.
type SimpleHandler struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// HookCommand is a single executable hook command.
type HookCommand struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Payload structs below decode only the fields the integration consumes.
// agy sends more (workspacePaths, artifactDirectoryPath, stepIdx,
// initialNumSteps, executionNum, terminationReason, error) — see the agy hooks
// docs for the full schema; add fields here only when something reads them.

// CommonPayload contains the system metadata fields the integration consumes
// from every hook payload.
type CommonPayload struct {
	ConversationID string `json:"conversationId"`
	TranscriptPath string `json:"transcriptPath"`
}

// ToolCall represents a proposed or completed tool invocation.
type ToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// PreToolUsePayload is the stdin payload for the PreToolUse hook.
type PreToolUsePayload struct {
	CommonPayload

	ToolCall ToolCall `json:"toolCall"`
}

// InvocationPayload is the stdin payload for the PreInvocation hook.
//
// invocationNum is 0-indexed (the first model invocation of a conversation is
// 0). initialNumSteps is deliberately not decoded: agy inserts the user prompt
// as a step before the first model call, so it is already 1 on the first
// invocation and unusable as a "first?" signal.
type InvocationPayload struct {
	CommonPayload

	InvocationNum int `json:"invocationNum"`
}

// StopPayload is the stdin payload for the Stop hook.
type StopPayload struct {
	CommonPayload

	FullyIdle bool `json:"fullyIdle"` // Required
}
