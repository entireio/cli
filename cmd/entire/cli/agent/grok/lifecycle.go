package grok

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ParseHookEvent translates a Grok hook into a normalized lifecycle Event.
// Returns nil when the hook carries no lifecycle significance.
func (g *GrokAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return g.parseSessionStart(ctx, stdin)
	case HookNameUserPromptSubmit:
		return g.parseTurnStart(stdin)
	case HookNameStop:
		return g.parseStop(stdin)
	case HookNameStopCancelled:
		return g.parseStopCancelled(stdin)
	case HookNameStopFailure:
		return g.parseStopFailure(stdin)
	case HookNameSessionEnd:
		return g.parseSessionEnd(stdin)
	case HookNamePreCompact:
		return g.parseCompaction(stdin)
	case HookNameSubagentStart:
		return g.parseSubagentStart(stdin)
	case HookNameSubagentStop:
		return g.parseSubagentStop(stdin)
	case HookNamePostToolUse:
		return g.parseToolUse(stdin)
	default:
		return nil, nil //nolint:nilnil // unknown hooks have no lifecycle action
	}
}

// parseTime converts Grok's RFC3339 timestamp, falling back to now.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

// sessionRef resolves the transcript path. Every hook but session_start
// supplies transcriptPath outright, which is authoritative; only session_start
// needs the path derived from cwd + session id.
func (g *GrokAgent) sessionRef(ctx context.Context, in baseHookInput) string {
	if in.TranscriptPath != "" {
		return in.TranscriptPath
	}
	// cwd, not workspaceRoot. Grok names the session group after the working
	// directory exactly as given, and the two differ: workspaceRoot carries a
	// trailing slash where cwd does not. Encoding that slash yields a group
	// name ending in %2F, which no session directory matches. The trailing
	// separator is trimmed anyway so the fallback stays correct if only
	// workspaceRoot is present.
	cwd := strings.TrimRight(in.CWD, "/")
	if cwd == "" {
		cwd = strings.TrimRight(in.WorkspaceRoot, "/")
	}
	if cwd == "" || in.SessionID == "" {
		return ""
	}
	dir, err := g.GetSessionDir(cwd)
	if err != nil {
		logging.Warn(ctx, "grok: failed to resolve session dir", "err", err)
		return ""
	}
	return g.ResolveSessionFile(dir, in.SessionID)
}

func (g *GrokAgent) parseSessionStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionStartInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  raw.SessionID,
		SessionRef: g.sessionRef(ctx, raw.baseHookInput),
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

func (g *GrokAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Prompt:     unwrapPrompt(raw.Prompt),
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

// userQueryOpen and userQueryClose bracket the prompt in Grok's
// user_prompt_submit payload.
const (
	userQueryOpen  = "<user_query>"
	userQueryClose = "</user_query>"
)

// unwrapPrompt strips the <user_query> envelope Grok wraps the prompt in.
//
// The wrapper is Grok's own framing for the model, not something the user
// typed, and Prompt is user-facing: it becomes the checkpoint label, the
// `entire status` line, and search text. Left in, every Grok checkpoint reads
// "<user_query> Create a file...". Anything outside the envelope is preserved
// rather than discarded, so a payload that is not wrapped, or is wrapped
// alongside extra context, survives unchanged.
func unwrapPrompt(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	open := strings.Index(trimmed, userQueryOpen)
	if open == -1 {
		return prompt
	}
	closeIdx := strings.LastIndex(trimmed, userQueryClose)
	if closeIdx == -1 || closeIdx < open {
		return prompt
	}
	before := strings.TrimSpace(trimmed[:open])
	inner := strings.TrimSpace(trimmed[open+len(userQueryOpen) : closeIdx])
	after := strings.TrimSpace(trimmed[closeIdx+len(userQueryClose):])

	parts := make([]string, 0, 3)
	for _, p := range []string{before, inner, after} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return prompt
	}
	return strings.Join(parts, "\n\n")
}

// isTeardownStop reports whether a Stop payload is the observe-only one Grok
// fires at session teardown rather than at the end of a turn.
//
// Grok fires Stop twice per session: once for the turn (reason "end_turn",
// promptId set, lastAssistantMessage populated) and once during shutdown
// (reason "shutdown", no promptId). Treating the second as a TurnEnd mints a
// duplicate, contentless checkpoint on every session. Both signals are checked
// because reason is documented to also take "channel_closed" at teardown, and
// only the turn-scoped payload carries a promptId.
func isTeardownStop(reason, promptID string) bool {
	return reason == "shutdown" || reason == "channel_closed" || promptID == ""
}

func (g *GrokAgent) parseStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopInput](stdin)
	if err != nil {
		return nil, err
	}
	if isTeardownStop(raw.Reason, raw.PromptID) {
		return nil, nil //nolint:nilnil // teardown Stop is observe-only; SessionEnd covers it
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

// parseStopCancelled maps an interrupted turn to TurnEnd.
//
// Grok fires StopCancelled INSTEAD of Stop when a turn is interrupted, has a
// permission prompt declined, hits --max-turns, or bails out for lack of
// progress. Without this mapping every such turn's work goes uncheckpointed,
// which is the common case for a user pressing Ctrl+C mid-edit.
func (g *GrokAgent) parseStopCancelled(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopCancelledInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

// parseStopFailure maps an API-errored turn to TurnEnd, for the same reason as
// parseStopCancelled: Stop does not fire.
func (g *GrokAgent) parseStopFailure(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopFailureInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

func (g *GrokAgent) parseSessionEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionEndInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

func (g *GrokAgent) parseCompaction(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[compactInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.Compaction,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  parseTime(raw.Timestamp),
	}, nil
}

func (g *GrokAgent) parseSubagentStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:         agent.SubagentStart,
		SessionID:    raw.SessionID,
		SessionRef:   raw.TranscriptPath,
		SubagentType: raw.SubagentType,
		Timestamp:    parseTime(raw.Timestamp),
	}, nil
}

// parseSubagentStop maps Grok's subagent completion to SubagentEnd.
//
// Final is true because Grok fires SubagentStop once, at real completion —
// there is no separate launch stub to disambiguate from, unlike Claude Code's
// two-signal background-task model.
func (g *GrokAgent) parseSubagentStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:         agent.SubagentEnd,
		SessionID:    raw.SessionID,
		SessionRef:   raw.TranscriptPath,
		SubagentType: raw.SubagentType,
		Final:        true,
		Timestamp:    parseTime(raw.Timestamp),
	}, nil
}

// parseToolUse reports mid-turn file writes so the framework can attribute
// files to a commit the agent makes before its turn ends.
//
// Returns nil for tools that touched no file, which is most of them.
func (g *GrokAgent) parseToolUse(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[toolUseInput](stdin)
	if err != nil {
		return nil, err
	}
	path := toolUsePath(raw)
	if path == "" {
		return nil, nil //nolint:nilnil // no file touched, no lifecycle action
	}
	return &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     raw.SessionID,
		SessionRef:    raw.TranscriptPath,
		ToolUseID:     raw.ToolUseID,
		CWD:           raw.CWD,
		ModifiedFiles: []string{path},
		Timestamp:     parseTime(raw.Timestamp),
	}, nil
}

// toolUsePath extracts the file a write/edit tool touched, preferring the
// absolute path in the result over the repo-relative one in the input.
func toolUsePath(raw *toolUseInput) string {
	if len(raw.ToolResult) > 0 {
		var res toolResultPaths
		if err := json.Unmarshal(raw.ToolResult, &res); err == nil {
			if p := res.EditsApplied.AbsolutePath; p != "" {
				return p
			}
		}
	}
	if len(raw.ToolInput) > 0 {
		var in toolInputPaths
		if err := json.Unmarshal(raw.ToolInput, &in); err == nil {
			if in.FilePath != "" {
				return in.FilePath
			}
			if in.Path != "" {
				return in.Path
			}
		}
	}
	return ""
}
