package copilotcli

import (
	"context"
	"fmt"
	"io"

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
//
// For VS Code payloads (detected via hookEventName), the event name is validated
// against the CLI subcommand. Mismatches are silently skipped to avoid processing
// a payload that doesn't match the hook being invoked.
func (c *CopilotCLIAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	// Pass-through hooks: skip immediately without reading stdin.
	switch hookName {
	case HookNamePreToolUse, HookNamePostToolUse, HookNameErrorOccurred:
		return nil, nil //nolint:nilnil // Pass-through hooks have no lifecycle action
	}

	// For lifecycle hooks, read and parse the envelope first so we can
	// validate VS Code hookEventName before constructing an event.
	env, err := c.readHookEnvelope(stdin)
	if err != nil {
		return nil, err
	}

	// VS Code payloads: validate hookEventName matches the CLI subcommand.
	if env.Host == HostVSCode && env.HookEventName != "" {
		if !validateVSCodeEvent(env, hookName) {
			logging.Debug(ctx, "copilot-cli: skipping VS Code event with mismatched hookEventName",
				"hookEventName", env.HookEventName, "hookName", hookName)
			return nil, nil //nolint:nilnil // Mismatched VS Code event — skip silently.
		}
	}

	switch hookName {
	case HookNameUserPromptSubmitted:
		return c.buildUserPromptSubmitted(ctx, env), nil
	case HookNameSessionStart:
		return c.buildSessionStart(env), nil
	case HookNameAgentStop:
		return c.buildAgentStop(ctx, env), nil
	case HookNameSessionEnd:
		return c.buildSessionEnd(env), nil
	case HookNameSubagentStop:
		return c.buildSubagentStop(env), nil
	default:
		logging.Debug(ctx, "copilot-cli: ignoring unknown hook", "hook", hookName)
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// --- Internal event builders (envelope already parsed) ---

func (c *CopilotCLIAgent) buildUserPromptSubmitted(ctx context.Context, env *hookEnvelope) *agent.Event {
	transcriptRef := env.TranscriptPath
	if transcriptRef == "" {
		transcriptRef = c.resolveTranscriptRef(ctx, env.SessionID)
	}

	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  env.SessionID,
		SessionRef: transcriptRef,
		Prompt:     env.Prompt,
		Timestamp:  env.Timestamp,
	}
}

func (c *CopilotCLIAgent) buildSessionStart(env *hookEnvelope) *agent.Event {
	return &agent.Event{
		Type:      agent.SessionStart,
		SessionID: env.SessionID,
		Timestamp: env.Timestamp,
	}
}

func (c *CopilotCLIAgent) buildAgentStop(ctx context.Context, env *hookEnvelope) *agent.Event {
	var model string
	if env.TranscriptPath != "" {
		model = ExtractModelFromTranscript(ctx, env.TranscriptPath)
	}

	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  env.SessionID,
		SessionRef: env.TranscriptPath,
		Model:      model,
		Timestamp:  env.Timestamp,
	}
}

func (c *CopilotCLIAgent) buildSessionEnd(env *hookEnvelope) *agent.Event {
	return &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: env.SessionID,
		Timestamp: env.Timestamp,
	}
}

func (c *CopilotCLIAgent) buildSubagentStop(env *hookEnvelope) *agent.Event {
	return &agent.Event{
		Type:      agent.SubagentEnd,
		SessionID: env.SessionID,
		Timestamp: env.Timestamp,
	}
}

func (c *CopilotCLIAgent) readHookEnvelope(stdin io.Reader) (*hookEnvelope, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to read hook input: %w", err)
	}
	return parseHookEnvelope(data)
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
