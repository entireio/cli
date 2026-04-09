package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions.
var (
	_ agent.HookSupport        = (*CodexAgent)(nil)
	_ agent.HookResponseWriter = (*CodexAgent)(nil)
)

// WriteHookResponse outputs a JSON hook response to stdout.
// Codex reads the systemMessage field and displays it to the user.
func (c *CodexAgent) WriteHookResponse(message string) error {
	resp := struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}{SystemMessage: message}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return fmt.Errorf("failed to encode hook response: %w", err)
	}
	return nil
}

// Codex hook names — these become subcommands under `entire hooks codex`
const (
	HookNameSessionStart     = "session-start"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNameStop             = "stop"
	HookNamePreToolUse       = "pre-tool-use"
)

// HookNames returns the hook verbs Codex supports.
func (c *CodexAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameUserPromptSubmit,
		HookNameStop,
		HookNamePreToolUse,
	}
}

// ParseHookEvent translates a Codex hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (c *CodexAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionStart(stdin)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(stdin)
	case HookNameStop:
		return c.parseTurnEnd(stdin)
	case HookNamePreToolUse:
		// PreToolUse has no lifecycle significance — pass through
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

func (c *CodexAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionStartRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  raw.SessionID,
		SessionRef: derefString(raw.TranscriptPath),
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CodexAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: derefString(raw.TranscriptPath),
		Prompt:     raw.Prompt,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CodexAgent) parseTurnEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: derefString(raw.TranscriptPath),
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}
