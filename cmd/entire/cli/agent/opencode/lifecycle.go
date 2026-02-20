package opencode

import (
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Hook name constants — these become CLI subcommands under `entire hooks opencode`.
const (
	HookNameSessionStart = "session-start"
	HookNameSessionEnd   = "session-end"
	HookNameTurnStart    = "turn-start"
	HookNameTurnEnd      = "turn-end"
	HookNameCompaction   = "compaction"
)

// HookNames returns the hook verbs this agent supports.
func (a *OpenCodeAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
		HookNameCompaction,
	}
}

// ParseHookEvent translates OpenCode hook calls into normalized lifecycle events.
func (a *OpenCodeAgent) ParseHookEvent(hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.SessionStart,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	case HookNameTurnStart:
		raw, err := agent.ReadAndParseHookInput[turnStartRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnStart,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Prompt:     raw.Prompt,
			Timestamp:  time.Now(),
		}, nil

	case HookNameTurnEnd:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	case HookNameCompaction:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.Compaction,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	case HookNameSessionEnd:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.SessionEnd,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	default:
		return nil, nil //nolint:nilnil // nil event = no lifecycle action for unknown hooks
	}
}
