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
	_ agent.HookSupport                = (*CodexAgent)(nil)
	_ agent.HookResponseWriter         = (*CodexAgent)(nil)
	_ agent.ContextInjector            = (*CodexAgent)(nil)
	_ agent.SubagentTranscriptResolver = (*CodexAgent)(nil)
)

// metaKeyAgentTranscriptPath carries the SubagentStop hook's agent_transcript_path
// (the child rollout path) on the Event so ResolveSubagentTranscript can return
// it without globbing the sessions tree.
const metaKeyAgentTranscriptPath = "agent_transcript_path"

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
	HookNameSubagentStart    = "subagent-start"
	HookNameSubagentStop     = "subagent-stop"
)

// HookNames returns the hook verbs Codex supports.
func (c *CodexAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameUserPromptSubmit,
		HookNameStop,
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNameSubagentStart,
		HookNameSubagentStop,
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
	case HookNameSubagentStart:
		return c.parseSubagentStart(stdin)
	case HookNameSubagentStop:
		return c.parseSubagentStop(stdin)
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

// parseSubagentStart maps Codex's SubagentStart hook to a SubagentStart event.
// Codex has no per-task tool_use_id, so the subagent's agent_id (a path-safe
// UUID) doubles as the ToolUseID that keys the task's pre-state and checkpoint.
func (c *CodexAgent) parseSubagentStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStartRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:         agent.SubagentStart,
		SessionID:    raw.SessionID,
		SessionRef:   derefString(raw.TranscriptPath),
		Model:        raw.Model,
		ToolUseID:    raw.AgentID,
		SubagentID:   raw.AgentID,
		SubagentType: raw.AgentType,
		Timestamp:    time.Now(),
	}, nil
}

// parseSubagentStop maps Codex's SubagentStop hook to a SubagentEnd event. The
// hook supplies agent_transcript_path (the child rollout) directly; it is
// stashed in Metadata so ResolveSubagentTranscript can return it.
func (c *CodexAgent) parseSubagentStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStopRaw](stdin)
	if err != nil {
		return nil, err
	}
	evt := &agent.Event{
		Type:         agent.SubagentEnd,
		SessionID:    raw.SessionID,
		SessionRef:   derefString(raw.TranscriptPath),
		Model:        raw.Model,
		ToolUseID:    raw.AgentID,
		SubagentID:   raw.AgentID,
		SubagentType: raw.AgentType,
		Timestamp:    time.Now(),
	}
	if p := derefString(raw.AgentTranscriptPath); p != "" {
		evt.Metadata = map[string]string{metaKeyAgentTranscriptPath: p}
	}
	return evt, nil
}

// ResolveSubagentTranscript returns the child rollout path for a Codex subagent.
// Codex child rollouts live under CODEX_HOME/sessions/... (not next to the main
// transcript), so the SubagentStop hook's agent_transcript_path is preferred;
// otherwise the sessions tree is globbed by the subagent's agent_id.
func (c *CodexAgent) ResolveSubagentTranscript(event *agent.Event) string {
	if event == nil {
		return ""
	}
	if p := event.Metadata[metaKeyAgentTranscriptPath]; p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		// The hook-provided path is stale/moved/archived — fall through to globbing
		// the sessions tree (incl. archived_sessions) by the subagent's agent_id
		// rather than returning a dead path.
	}
	if event.SubagentID == "" {
		return ""
	}
	sessionDir, err := c.GetSessionDir("")
	if err != nil {
		return ""
	}
	return findRolloutBySessionID(sessionDir, event.SubagentID)
}
