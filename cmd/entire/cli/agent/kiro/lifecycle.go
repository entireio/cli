package kiro

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// ParseHookEvent translates a Kiro hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (k *KiroAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameAgentSpawn:
		return k.parseSessionStart(stdin)
	case HookNameUserPromptSubmit:
		return k.parseTurnStart(stdin)
	case HookNameStop:
		return k.parseTurnEnd(stdin)
	case HookNamePreToolUse:
		return k.parsePreToolUse(stdin)
	case HookNamePostToolUse:
		return k.parsePostToolUse(stdin)
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// ReadTranscript reads the raw transcript bytes for a session.
func (k *KiroAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// Note: KiroAgent does NOT implement TranscriptAnalyzer. Kiro uses SQLite
// for sessions, so transcript-based file extraction is not available.
// File detection relies on git status instead.

// --- Internal hook parsing functions ---

func (k *KiroAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[agentSpawnRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:      agent.SessionStart,
		SessionID: raw.SessionID,
		Timestamp: time.Now(),
	}, nil
}

func (k *KiroAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:      agent.TurnStart,
		SessionID: raw.SessionID,
		Prompt:    raw.Prompt,
		Timestamp: time.Now(),
	}, nil
}

func (k *KiroAgent) parseTurnEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:      agent.TurnEnd,
		SessionID: raw.SessionID,
		Timestamp: time.Now(),
	}, nil
}

func (k *KiroAgent) parsePreToolUse(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[preToolUseRaw](stdin)
	if err != nil {
		return nil, err
	}
	// Only map to SubagentStart for delegation-like tools
	if raw.ToolName == "" {
		return nil, nil //nolint:nilnil // No tool name means no lifecycle action
	}
	return &agent.Event{
		Type:      agent.SubagentStart,
		SessionID: raw.SessionID,
		ToolUseID: raw.ToolUseID,
		Timestamp: time.Now(),
	}, nil
}

func (k *KiroAgent) parsePostToolUse(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[postToolUseRaw](stdin)
	if err != nil {
		return nil, err
	}
	if raw.ToolName == "" {
		return nil, nil //nolint:nilnil // No tool name means no lifecycle action
	}
	return &agent.Event{
		Type:      agent.SubagentEnd,
		SessionID: raw.SessionID,
		ToolUseID: raw.ToolUseID,
		Timestamp: time.Now(),
	}, nil
}
