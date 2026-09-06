package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Compile-time interface assertions.
var (
	_ agent.HookSupport              = (*CodexAgent)(nil)
	_ agent.HookConfigLocator        = (*CodexAgent)(nil)
	_ agent.HookFreshness            = (*CodexAgent)(nil)
	_ agent.EffectiveHookDiagnostics = (*CodexAgent)(nil)
	_ agent.HookResponseWriter       = (*CodexAgent)(nil)
	_ agent.ContextInjector          = (*CodexAgent)(nil)
	_ agent.SessionEndBudgeter       = (*CodexAgent)(nil)
)

// OwnsEffectiveHookDiagnostics keeps Codex's discovered-file state out of the
// generic current-worktree freshness report.
func (c *CodexAgent) OwnsEffectiveHookDiagnostics() {}

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
	HookNameSessionEnd       = "session-end"
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
		HookNameSessionEnd,
		HookNameUserPromptSubmit,
		HookNameStop,
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNameSubagentStart,
		HookNameSubagentStop,
	}
}

// SessionEndTimeoutSec is the timeout Entire configures for Codex's SessionEnd
// hook. Codex clamps SessionEnd handlers to
// SESSION_END_MAX_TIMEOUT_SEC = 3 (codex-rs/hooks/src/events/session_end.rs) and
// prints a "clamping SessionEnd hook timeout" warning on every startup when a
// config asks for more, so requesting exactly the ceiling gets the longest run
// available without nagging the user. Every other Codex hook keeps the
// standard 30s that addHook applies.
const SessionEndTimeoutSec = 3

// sessionEndBudget is how long the whole session-end hook process may run
// before Codex kills its process tree. Held under the configured
// SessionEndTimeoutSec so Entire stops itself cleanly instead of being
// terminated part-way through a condense.
//
// The gap is 1s rather than the tighter 500ms it started as, because the two
// clocks do not start together. Codex's cap starts when it spawns the wrapper
// (`sh -c 'if ! command -v entire …; exec entire hooks codex session-end'`),
// while ours starts at Go package init (processStart). Everything in between —
// sh startup, the command -v PATH walk, and loading a ~66MB binary — is spent
// before we can measure anything. Warm that is ~40ms, but a cold page cache, a
// network filesystem, or an on-access scanner is exactly the case where it is
// not, and overrunning means the process tree is killed mid-condense rather
// than stopping short of it.
const sessionEndBudget = 2 * time.Second

// SessionEndBudget implements agent.SessionEndBudgeter. Codex runs SessionEnd
// inside its shutdown sequence under a hard cap; see the interface docs.
func (c *CodexAgent) SessionEndBudget() time.Duration { return sessionEndBudget }

// ParseHookEvent translates a Codex hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (c *CodexAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionInfoEvent(stdin, agent.SessionStart)
	case HookNameSessionEnd:
		return c.parseSessionInfoEvent(stdin, agent.SessionEnd)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(ctx, stdin)
	case HookNameStop:
		return c.parseTurnEnd(ctx, stdin)
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

// parseSubagentStart maps Codex's SubagentStart (thread-spawned subagents only;
// internal/synthetic ones expose no user-configured hooks).
//
// session_id is the identity shared by the root thread and all descendants — the
// user's session — while agent_id is the child thread's own. Codex sends no
// tool_use_id, so agent_id doubles as ToolUseID, the key Entire correlates
// start/stop on. See AGENT.md for the full contract.
func (c *CodexAgent) parseSubagentStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStartRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:         agent.SubagentStart,
		SessionID:    raw.SessionID,
		SessionRef:   derefString(raw.TranscriptPath),
		ToolUseID:    raw.AgentID,
		TurnID:       raw.TurnID,
		SubagentID:   raw.AgentID,
		SubagentType: raw.AgentType,
		Model:        raw.Model,
		Timestamp:    time.Now(),
	}, nil
}

// parseSubagentStop maps Codex's SubagentStop. Note the two transcripts:
// transcript_path is the parent thread's rollout, agent_transcript_path the
// subagent's own (see Event.SubagentTranscriptPath).
func (c *CodexAgent) parseSubagentStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStopRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:                    agent.SubagentEnd,
		SessionID:               raw.SessionID,
		SessionRef:              derefString(raw.TranscriptPath),
		ToolUseID:               raw.AgentID,
		TurnID:                  raw.TurnID,
		SubagentID:              raw.AgentID,
		ProvisionalSubagentStop: true,
		SubagentType:            raw.AgentType,
		SubagentTranscriptPath:  derefString(raw.AgentTranscriptPath),
		Model:                   raw.Model,
		Timestamp:               time.Now(),
	}, nil
}

// parseSessionInfoEvent parses the hooks whose payload is sessionInfoRaw —
// SessionStart and SessionEnd differ only in the event type. Model is empty on
// SessionEnd (Codex omits it there); the session state already recorded the
// model at turn start.
func (c *CodexAgent) parseSessionInfoEvent(stdin io.Reader, eventType agent.EventType) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       eventType,
		SessionID:  raw.SessionID,
		SessionRef: derefString(raw.TranscriptPath),
		CWD:        raw.CWD,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CodexAgent) parseTurnStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
	if err != nil {
		return nil, err
	}
	if !isRootTurnRollout(ctx, derefString(raw.TranscriptPath)) {
		return nil, nil //nolint:nilnil // only confirmed child rollouts are skipped
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

func (c *CodexAgent) parseTurnEnd(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopRaw](stdin)
	if err != nil {
		return nil, err
	}
	if !isRootTurnRollout(ctx, derefString(raw.TranscriptPath)) {
		return nil, nil //nolint:nilnil // only confirmed child rollouts are skipped
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: derefString(raw.TranscriptPath),
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func isRootTurnRollout(ctx context.Context, path string) bool {
	classification := classifyRolloutDetailed(path)
	switch classification.Classification {
	case rolloutRoot:
		return true
	case rolloutChild:
		logging.Debug(ctx, "codex: skipped root lifecycle mutation for child rollout", slog.String("path", path))
		return false
	case rolloutUnknown:
		logging.Warn(ctx, "codex: preserved root lifecycle event because rollout ownership is unverified",
			slog.String("category", string(classification.Issue)),
			slog.String("detail", classification.Detail),
			slog.String("path", path))
		return true
	}
	return false
}
