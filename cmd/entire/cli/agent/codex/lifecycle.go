package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/internal/hookcompat"
)

// Compile-time interface assertions.
var (
	_ agent.HookSupport        = (*CodexAgent)(nil)
	_ agent.HookResponseWriter = (*CodexAgent)(nil)
	_ agent.ContextInjector    = (*CodexAgent)(nil)
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

// InjectionEvent reports that Codex injects model context at TurnStart (its
// user-prompt-submit hook). Codex hosts Claude-compatible hooks, so it consumes
// the same hookSpecificOutput.additionalContext shape.
func (c *CodexAgent) InjectionEvent() agent.EventType { return agent.TurnStart }

// RenderContextInjection renders the Claude-style additionalContext payload
// Codex injects into the model context at user-prompt-submit.
func (c *CodexAgent) RenderContextInjection(inj agent.ContextInjection) ([]byte, error) {
	out, err := agent.RenderAdditionalContextHookOutput("UserPromptSubmit", inj.Text)
	if err != nil {
		return nil, fmt.Errorf("render codex context injection: %w", err)
	}
	return out, nil
}

// Codex hook names — these become subcommands under `entire hooks codex`
const (
	HookNameSessionStart     = "session-start"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNameStop             = "stop"
	HookNamePreToolUse       = "pre-tool-use"
	HookNamePostToolUse      = "post-tool-use"
)

var codexHookEventToHookNames = map[string][]string{
	"SessionStart":     {HookNameSessionStart},
	"UserPromptSubmit": {HookNameUserPromptSubmit},
	"Stop":             {HookNameStop},
	"PreToolUse":       {HookNamePreToolUse},
	"PostToolUse":      {HookNamePostToolUse},
}

// HookNames returns the hook verbs Codex supports.
func (c *CodexAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameUserPromptSubmit,
		HookNameStop,
		HookNamePreToolUse,
		HookNamePostToolUse,
	}
}

// ParseHookEvent translates a Codex hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (c *CodexAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionStart(stdin, hookName)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(stdin, hookName)
	case HookNameStop:
		return c.parseTurnEnd(stdin, hookName)
	case HookNamePreToolUse:
		// PreToolUse has no lifecycle significance — pass through
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	case HookNamePostToolUse:
		return c.parsePostToolUse(stdin, hookName)
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

func (c *CodexAgent) parseSessionStart(stdin io.Reader, hookName string) (*agent.Event, error) {
	env, ok, err := readCompatEnvelope(stdin, hookName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil //nolint:nilnil // Mismatched plugin event — skip silently.
	}
	return &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  env.SessionID,
		SessionRef: env.TranscriptPath,
		Model:      env.Model,
		Timestamp:  env.Timestamp,
	}, nil
}

func (c *CodexAgent) parseTurnStart(stdin io.Reader, hookName string) (*agent.Event, error) {
	env, ok, err := readCompatEnvelope(stdin, hookName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil //nolint:nilnil // Mismatched plugin event — skip silently.
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  env.SessionID,
		SessionRef: env.TranscriptPath,
		Prompt:     env.Prompt,
		Model:      env.Model,
		Timestamp:  env.Timestamp,
	}, nil
}

// Codex PostToolUse tool_name values that represent file mutations. The
// canonical Codex name is apply_patch; Write and Edit are matcher aliases
// Codex registers for compatibility with Claude-style hook configs — see
// codex-rs/core/src/tools/hook_names.rs:apply_patch.
const (
	toolNameApplyPatch = "apply_patch"
	toolAliasWrite     = "Write"
	toolAliasEdit      = "Edit"
)

// parsePostToolUse turns a Codex PostToolUse hook into a ToolUse lifecycle event.
// Non-mutating tools (shell, MCP) produce a nil event so the dispatcher skips
// them — extracting files from arbitrary shell commands would be unreliable.
func (c *CodexAgent) parsePostToolUse(stdin io.Reader, hookName string) (*agent.Event, error) {
	raw, err := hookcompat.ReadRaw(stdin)
	if err != nil {
		return nil, err
	}
	env, err := hookcompat.EnvelopeFromRaw(raw)
	if err != nil {
		return nil, err
	}
	if !hookcompat.HookEventMatches(env.HookEventName, hookName, codexHookEventToHookNames) {
		return nil, nil //nolint:nilnil // Mismatched plugin event — skip silently.
	}

	toolName := hookcompat.FirstString(raw, "tool_name", "toolName")
	if !isApplyPatchTool(toolName) {
		return nil, nil //nolint:nilnil // non-mutating tools have no lifecycle action
	}

	var input applyPatchToolInput
	// Best-effort: an unparseable tool_input means we can't extract files, but
	// we shouldn't fail the hook (which would block the agent's tool call).
	_ = json.Unmarshal(hookcompat.FirstRaw(raw, "tool_input", "toolInput"), &input) //nolint:errcheck // input.Command stays empty on failure

	added, modified, deleted := classifyApplyPatchPaths(input.Command)
	if len(added) == 0 && len(modified) == 0 && len(deleted) == 0 {
		return nil, nil //nolint:nilnil // empty or unparseable envelope
	}

	return &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     env.SessionID,
		SessionRef:    env.TranscriptPath,
		Model:         env.Model,
		ToolUseID:     hookcompat.FirstString(raw, "tool_use_id", "toolUseId", "toolUseID"),
		CWD:           env.CWD,
		ModifiedFiles: modified,
		NewFiles:      added,
		DeletedFiles:  deleted,
		Timestamp:     env.Timestamp,
	}, nil
}

func isApplyPatchTool(name string) bool {
	switch name {
	case toolNameApplyPatch, toolAliasWrite, toolAliasEdit:
		return true
	default:
		return false
	}
}

func (c *CodexAgent) parseTurnEnd(stdin io.Reader, hookName string) (*agent.Event, error) {
	env, ok, err := readCompatEnvelope(stdin, hookName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil //nolint:nilnil // Mismatched plugin event — skip silently.
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  env.SessionID,
		SessionRef: env.TranscriptPath,
		Model:      env.Model,
		Timestamp:  env.Timestamp,
	}, nil
}

func readCompatEnvelope(stdin io.Reader, hookName string) (*hookcompat.Envelope, bool, error) {
	env, err := hookcompat.ReadEnvelope(stdin)
	if err != nil {
		return nil, false, err
	}
	if !hookcompat.HookEventMatches(env.HookEventName, hookName, codexHookEventToHookNames) {
		return env, false, nil
	}
	return env, true, nil
}
