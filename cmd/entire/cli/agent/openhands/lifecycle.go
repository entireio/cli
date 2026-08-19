package openhands

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// Compile-time assertion.
var _ agent.HookSupport = (*OpenHandsAgent)(nil)

// Hook verbs — these become subcommands under `entire hooks openhands`.
const (
	HookNameSessionStart = "session-start"
	HookNameSessionEnd   = "session-end"
	HookNameTurnStart    = "turn-start"
	HookNameTurnEnd      = "turn-end"
)

// openhandsHookEvents maps each Entire hook verb to the OpenHands config key
// that fires it. Keys are the canonical snake_case field names from
// openhands/sdk/hooks/config.py; hooks.go renders the file from this map.
var openhandsHookEvents = map[string]string{
	HookNameSessionStart: "session_start",
	HookNameTurnStart:    "user_prompt_submit",
	HookNameTurnEnd:      "stop",
	HookNameSessionEnd:   "session_end",
}

// HookNames returns the hook verbs this agent supports.
func (a *OpenHandsAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
	}
}

// ParseHookEvent translates an OpenHands hook invocation into a lifecycle event.
//
// OpenHands supplies no transcript path, so the conversation's event directory
// is reconstructed from session_id. The verb identifies the event rather than
// the payload's event_type, so a renamed field cannot break parsing.
func (a *OpenHandsAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
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

	if eventType == agent.TurnStart || eventType == agent.TurnEnd {
		ref, refErr := eventDirFor(raw.SessionID)
		if refErr != nil {
			return nil, refErr
		}
		event.SessionRef = ref
	}
	if eventType == agent.TurnStart {
		event.Prompt = raw.Message
	}
	return event, nil
}

// eventDirFor validates a conversation id and returns its event directory.
func eventDirFor(sessionID string) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID for transcript path: %w", err)
	}
	root, err := conversationsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, conversationDirID(sessionID), eventsDirName), nil
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
