package vogon

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions.
var (
	_ agent.HookSupport        = (*Agent)(nil)
	_ agent.HookResponseWriter = (*Agent)(nil)
)

const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
)

// HookNames returns the hooks the vogon agent supports.
func (v *Agent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
	}
}

// ParseHookEvent translates vogon agent hook JSON into a normalized lifecycle Event.
func (v *Agent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
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
			Model:      raw.Model,
			Timestamp:  time.Now(),
		}, nil

	case HookNameUserPromptSubmit:
		raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnStart,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Prompt:     raw.Prompt,
			Model:      raw.Model,
			Timestamp:  time.Now(),
		}, nil

	case HookNameStop:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Model:      raw.Model,
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
			Model:      raw.Model,
			Timestamp:  time.Now(),
		}, nil

	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// InstallHooks is a no-op — the vogon binary fires hooks directly.
func (v *Agent) InstallHooks(_ context.Context, _ bool, _ bool) (int, error) {
	return 0, nil
}

// UninstallHooks is a no-op.
func (v *Agent) UninstallHooks(_ context.Context) error { return nil }

// AreHooksInstalled returns false — vogon agent has no external hooks to install.
// The vogon binary fires hooks directly via `entire hooks vogon <verb>`.
func (v *Agent) AreHooksInstalled(_ context.Context) bool {
	return false
}

// WriteHookResponse writes a plain text message to stdout.
func (v *Agent) WriteHookResponse(message string) error {
	if _, err := fmt.Fprintln(os.Stdout, message); err != nil {
		return fmt.Errorf("write hook response: %w", err)
	}
	return nil
}

// Hook JSON types — same format as other agents for consistency.

type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model,omitempty"`
}

type userPromptSubmitRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
}
