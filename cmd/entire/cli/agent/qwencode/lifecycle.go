package qwencode

import (
	"context"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time assertion.
var _ agent.HookSupport = (*QwenCodeAgent)(nil)

// Hook verbs — these become subcommands under `entire hooks qwen-code`.
const (
	HookNameSessionStart = "session-start"
	HookNameSessionEnd   = "session-end"
	HookNameTurnStart    = "turn-start"
	HookNameTurnEnd      = "turn-end"
)

// qwenHookEvents maps each Entire hook verb to the Qwen event that fires it.
// hooks.go renders the settings file from this map, so the installed config and
// the parser cannot drift apart.
var qwenHookEvents = map[string]string{
	HookNameSessionStart: "SessionStart",
	HookNameTurnStart:    "UserPromptSubmit",
	HookNameTurnEnd:      "Stop",
	HookNameSessionEnd:   "SessionEnd",
}

// HookNames returns the hook verbs this agent supports.
func (a *QwenCodeAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
	}
}

// ParseHookEvent translates a Qwen hook invocation into a lifecycle event.
//
// Qwen supplies transcript_path on stdin, so SessionRef comes straight from the
// payload. No path is reconstructed and no export command runs, which is why
// this agent needs no TranscriptPreparer.
//
// The verb identifies the event rather than the payload's hook_event_name: the
// subcommand already encodes it, so a renamed field cannot break parsing.
func (a *QwenCodeAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	eventType, ok := lifecycleType(hookName)
	if !ok {
		return nil, nil //nolint:nilnil // nil event = no lifecycle action for unknown hooks
	}

	raw, err := agent.ReadAndParseHookInput[hookPayload](stdin)
	if err != nil {
		return nil, err
	}

	event := &agent.Event{
		Type:      eventType,
		SessionID: raw.SessionID,
		Timestamp: time.Now(),
	}

	// Only the turn events carry a transcript reference; the session-scoped
	// ones have no meaningful offset to attach it to.
	if eventType == agent.TurnStart || eventType == agent.TurnEnd {
		event.SessionRef = raw.TranscriptPath
	}
	if eventType == agent.TurnStart {
		event.Prompt = raw.Prompt
	}
	return event, nil
}

// lifecycleType maps a hook verb to its Entire event type.
func lifecycleType(hookName string) (agent.EventType, bool) {
	switch hookName {
	case HookNameSessionStart:
		return agent.SessionStart, true
	case HookNameTurnStart:
		return agent.TurnStart, true
	case HookNameTurnEnd:
		return agent.TurnEnd, true
	case HookNameSessionEnd:
		return agent.SessionEnd, true
	default:
		return 0, false
	}
}
