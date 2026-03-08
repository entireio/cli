package copilotcli

import (
	"context"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Ensure CopilotCLIAgent implements HookSupport at compile time.
var _ agent.HookSupport = (*CopilotCLIAgent)(nil)

// Copilot CLI hook names - these become subcommands under `entire hooks copilot-cli`
const (
	HookNameUserPromptSubmitted = "user-prompt-submitted"
	HookNameSessionStart        = "session-start"
	HookNameAgentStop           = "agent-stop"
	HookNameSessionEnd          = "session-end"
	HookNameSubagentStop        = "subagent-stop"
	HookNamePreToolUse          = "pre-tool-use"
	HookNamePostToolUse         = "post-tool-use"
	HookNameErrorOccurred       = "error-occurred"
)

// HookNames returns all hook verbs Copilot CLI supports.
// These become subcommands: entire hooks copilot-cli <verb>
func (c *CopilotCLIAgent) HookNames() []string {
	return []string{
		HookNameUserPromptSubmitted,
		HookNameSessionStart,
		HookNameAgentStop,
		HookNameSessionEnd,
		HookNameSubagentStop,
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNameErrorOccurred,
	}
}

// ParseHookEvent translates a Copilot CLI hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance (pass-through hooks).
func (c *CopilotCLIAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameUserPromptSubmitted:
		return c.parseUserPromptSubmitted(ctx, stdin)
	case HookNameSessionStart:
		return c.parseSessionStart(stdin)
	case HookNameAgentStop:
		return c.parseAgentStop(ctx, stdin)
	case HookNameSessionEnd:
		return c.parseSessionEnd(stdin)
	case HookNameSubagentStop:
		return c.parseSubagentStop(stdin)
	case HookNamePreToolUse, HookNamePostToolUse, HookNameErrorOccurred:
		return nil, nil //nolint:nilnil // Pass-through hooks have no lifecycle action
	default:
		logging.Debug(ctx, "copilot-cli: ignoring unknown hook", "hook", hookName)
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// --- Internal hook parsing functions ---

func (c *CopilotCLIAgent) parseUserPromptSubmitted(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmittedRaw](stdin)
	if err != nil {
		return nil, err
	}

	transcriptRef := c.resolveTranscriptRef(ctx, raw.SessionID)

	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: transcriptRef,
		Prompt:     raw.Prompt,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CopilotCLIAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionStartRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:      agent.SessionStart,
		SessionID: raw.SessionID,
		Timestamp: time.Now(),
	}, nil
}

func (c *CopilotCLIAgent) parseAgentStop(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[agentStopRaw](stdin)
	if err != nil {
		return nil, err
	}

	// Extract model from transcript (Copilot CLI hooks don't include model)
	var model string
	if raw.TranscriptPath != "" {
		model = ExtractModelFromTranscript(ctx, raw.TranscriptPath)
	}

	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CopilotCLIAgent) parseSessionEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionEndRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: raw.SessionID,
		Timestamp: time.Now(),
	}, nil
}

func (c *CopilotCLIAgent) parseSubagentStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStopRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:      agent.SubagentEnd,
		SessionID: raw.SessionID,
		Timestamp: time.Now(),
	}, nil
}

// resolveTranscriptRef computes the transcript path from the session ID.
// Copilot CLI stores transcripts at ~/.copilot/session-state/<sessionId>/events.jsonl.
// The userPromptSubmitted hook does not include a transcriptPath field, so we compute it.
func (c *CopilotCLIAgent) resolveTranscriptRef(ctx context.Context, sessionID string) string {
	// GetSessionDir ignores the repoPath parameter for Copilot CLI since session
	// state is always in ~/.copilot/session-state/ (not repo-specific).
	sessionDir, err := c.GetSessionDir("")
	if err != nil {
		logging.Warn(ctx, "copilot-cli: failed to resolve transcript path", "sessionID", sessionID, "err", err)
		return ""
	}
	return c.ResolveSessionFile(sessionDir, sessionID)
}
