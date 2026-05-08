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
	HookNamePostToolUse      = "post-tool-use"
)

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
		return c.parseSessionStart(stdin)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(stdin)
	case HookNameStop:
		return c.parseTurnEnd(stdin)
	case HookNamePreToolUse:
		// PreToolUse has no lifecycle significance — pass through
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	case HookNamePostToolUse:
		return c.parsePostToolUse(stdin)
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
func (c *CodexAgent) parsePostToolUse(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[postToolUseRaw](stdin)
	if err != nil {
		return nil, err
	}

	if !isApplyPatchTool(raw.ToolName) {
		return nil, nil //nolint:nilnil // non-mutating tools have no lifecycle action
	}

	var input applyPatchToolInput
	// Best-effort: an unparseable tool_input means we can't extract files, but
	// we shouldn't fail the hook (which would block the agent's tool call).
	_ = json.Unmarshal(raw.ToolInput, &input) //nolint:errcheck // input.Command stays empty on failure

	added, modified, deleted := classifyApplyPatchPaths(input.Command)
	if len(added) == 0 && len(modified) == 0 && len(deleted) == 0 {
		return nil, nil //nolint:nilnil // empty or unparseable envelope
	}

	return &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     raw.SessionID,
		SessionRef:    derefString(raw.TranscriptPath),
		Model:         raw.Model,
		ToolUseID:     raw.ToolUseID,
		CWD:           raw.CWD,
		ModifiedFiles: modified,
		NewFiles:      added,
		DeletedFiles:  deleted,
		Timestamp:     time.Now(),
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
