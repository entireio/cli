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
	HookNamePreTask          = "pre-task"
	HookNamePostTask         = "post-task"
)

// HookNames returns the hooks the vogon agent supports.
func (v *Agent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNamePreTask,
		HookNamePostTask,
	}
}

// ParseHookEvent translates vogon agent hook JSON into a normalized lifecycle Event.
func (v *Agent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return parseSessionInfoEvent(stdin, agent.SessionStart)

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
		return parseSessionInfoEvent(stdin, agent.TurnEnd)

	case HookNameSessionEnd:
		return parseSessionInfoEvent(stdin, agent.SessionEnd)

	case HookNamePreTask:
		raw, err := agent.ReadAndParseHookInput[taskRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:            agent.SubagentStart,
			SessionID:       raw.SessionID,
			SessionRef:      raw.TranscriptPath,
			ToolUseID:       raw.ToolUseID,
			SubagentType:    raw.SubagentType,
			TaskDescription: raw.Description,
			Timestamp:       time.Now(),
		}, nil

	case HookNamePostTask:
		raw, err := agent.ReadAndParseHookInput[taskRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:            agent.SubagentEnd,
			SessionID:       raw.SessionID,
			SessionRef:      raw.TranscriptPath,
			ToolUseID:       raw.ToolUseID,
			SubagentID:      raw.AgentID,
			SubagentType:    raw.SubagentType,
			TaskDescription: raw.Description,
			Timestamp:       time.Now(),
		}, nil

	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// parseSessionInfoEvent parses the hooks whose payload is sessionInfoRaw —
// SessionStart, Stop, and SessionEnd differ only in the resulting event type.
func parseSessionInfoEvent(stdin io.Reader, eventType agent.EventType) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       eventType,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
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

// taskRaw is the payload for pre-task / post-task. Unlike Claude Code, vogon
// passes the subagent type, description and agent ID as flat fields rather than
// nested under tool_input/tool_response — the dispatcher accepts either, and flat
// fields keep the fake agent's payload readable.
type taskRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	ToolUseID      string `json:"tool_use_id"`
	AgentID        string `json:"agent_id,omitempty"`
	SubagentType   string `json:"subagent_type,omitempty"`
	Description    string `json:"description,omitempty"`
}

type userPromptSubmitRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
}
