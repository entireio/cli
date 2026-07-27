package devin

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Compile-time interface assertions.
var (
	_ agent.HookSupport        = (*DevinAgent)(nil)
	_ agent.TranscriptAnalyzer = (*DevinAgent)(nil)
	_ agent.TranscriptPreparer = (*DevinAgent)(nil)
	_ agent.TokenCalculator    = (*DevinAgent)(nil)
	_ agent.ModelExtractor     = (*DevinAgent)(nil)
)

// Devin hook names - these become subcommands under `entire hooks devin`.
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNamePostToolUse      = "post-tool-use"
)

// HookNames returns the hook verbs Devin supports.
func (d *DevinAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNamePostToolUse,
	}
}

// ParseHookEvent translates a Devin hook into a normalized lifecycle Event.
// Devin payloads carry no transcript_path, so SessionRef is derived from the
// session ID via the canonical transcript location.
func (d *DevinAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return d.parseSessionInfoEvent(ctx, stdin, agent.SessionStart)
	case HookNameUserPromptSubmit:
		return d.parseTurnStart(ctx, stdin)
	case HookNameStop:
		return d.parseSessionInfoEvent(ctx, stdin, agent.TurnEnd)
	case HookNameSessionEnd:
		return d.parseSessionInfoEvent(ctx, stdin, agent.SessionEnd)
	case HookNamePostToolUse:
		return d.parsePostToolUse(ctx, stdin)
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// sessionRefForID computes the canonical transcript path for a session ID.
// Returns "" when the transcript directory cannot be resolved (home dir /
// APPDATA unavailable — pathological): SessionStart/TurnStart handlers treat
// that as degraded, while TurnEnd rejects the event since a checkpoint
// without a transcript reference cannot be saved.
func (d *DevinAgent) sessionRefForID(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	dir, err := d.GetSessionDir(repoRoot)
	if err != nil {
		return ""
	}
	return d.ResolveSessionFile(dir, sessionID)
}

func (d *DevinAgent) parseSessionInfoEvent(ctx context.Context, stdin io.Reader, eventType agent.EventType) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       eventType,
		SessionID:  raw.SessionID,
		SessionRef: d.sessionRefForID(ctx, raw.SessionID),
		Timestamp:  time.Now(),
	}, nil
}

func (d *DevinAgent) parseTurnStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: d.sessionRefForID(ctx, raw.SessionID),
		Prompt:     raw.Prompt,
		Timestamp:  time.Now(),
	}, nil
}

// parsePostToolUse maps file-modifying tool completions to ToolUse events so
// files touched mid-turn are tracked live. Devin's transcript is only written
// at session end (see AGENT.md), so hook-time tracking is the primary source
// of per-turn file accounting. Non-file tools are pass-through, and ALL
// failures are pass-through too: mid-turn tracking is best-effort (git status
// covers the fallback), and Devin has been observed to spawn secondary
// PostToolUse matcher groups without piping the payload — a hard error here
// would surface as hook failures in Devin's logs every turn.
func (d *DevinAgent) parsePostToolUse(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[postToolUseRaw](stdin)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Best-effort tracking — never fail the hook
	}
	if !isFileModificationTool(raw.ToolName) || !raw.ToolResponse.Success {
		return nil, nil //nolint:nilnil // No lifecycle action for non-file or failed tools
	}
	var input fileToolInput
	if err := json.Unmarshal(raw.ToolInput, &input); err != nil || input.FilePath == "" {
		return nil, nil //nolint:nilnil,nilerr // No usable path — nothing to track (best-effort)
	}
	return &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     raw.SessionID,
		SessionRef:    d.sessionRefForID(ctx, raw.SessionID),
		ToolUseID:     raw.ToolUseID,
		ModifiedFiles: []string{input.FilePath},
		Timestamp:     time.Now(),
	}, nil
}
