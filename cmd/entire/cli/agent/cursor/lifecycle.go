package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// intFromJSON safely converts a json.Number to int64, returning 0 for
// empty or non-numeric values (hook payloads may omit optional fields).
func intFromJSON(n json.Number) int64 {
	v, err := n.Int64()
	if err != nil {
		return 0
	}
	return v
}

// ParseHookEvent translates a Cursor hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (c *CursorAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionStart(stdin)
	case HookNameBeforeSubmitPrompt:
		return c.parseTurnStart(ctx, stdin)
	case HookNameStop:
		return c.parseTurnEnd(ctx, stdin)
	case HookNameSessionEnd:
		return c.parseSessionEnd(ctx, stdin)
	case HookNamePreCompact:
		return c.parsePreCompact(stdin)
	case HookNameSubagentStart:
		return c.parseSubagentStart(stdin)
	case HookNameSubagentStop:
		return c.parseSubagentStop(stdin)
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (c *CursorAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// --- Internal hook parsing functions ---

// resolveTranscriptRef returns the transcript path from the hook input, or computes
// it dynamically when the hook doesn't provide one (Cursor CLI pattern).
func (c *CursorAgent) resolveTranscriptRef(ctx context.Context, conversationID, rawPath string) string {
	if rawPath != "" {
		return rawPath
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Warn(ctx, "cursor: failed to get worktree root for transcript resolution", "err", err)
		return ""
	}

	sessionDir, err := c.GetSessionDir(repoRoot)
	if err != nil {
		logging.Warn(ctx, "cursor: failed to get session dir for transcript resolution", "err", err)
		return ""
	}

	return c.ResolveSessionFile(sessionDir, conversationID)
}

func (c *CursorAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionStartRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  raw.ConversationID,
		SessionRef: raw.TranscriptPath,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CursorAgent) parseTurnStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[beforeSubmitPromptInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.ConversationID,
		SessionRef: c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		Prompt:     raw.Prompt,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *CursorAgent) parseTurnEnd(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.ConversationID,
		SessionRef: c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		Model:      raw.Model,
		TurnCount:  int(intFromJSON(raw.LoopCount)),
		Timestamp:  time.Now(),
	}, nil
}

func (c *CursorAgent) parseSessionEnd(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionEndRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionEnd,
		SessionID:  raw.ConversationID,
		SessionRef: c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		Model:      raw.Model,
		DurationMs: intFromJSON(raw.DurationMs),
		Timestamp:  time.Now(),
	}, nil
}

func (c *CursorAgent) parsePreCompact(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[preCompactHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:              agent.Compaction,
		SessionID:         raw.ConversationID,
		SessionRef:        raw.TranscriptPath,
		ContextTokens:     int(intFromJSON(raw.ContextTokens)),
		ContextWindowSize: int(intFromJSON(raw.ContextWindowSize)),
		Timestamp:         time.Now(),
	}, nil
}

func (c *CursorAgent) parseSubagentStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStartHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	if raw.Task == "" {
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	}
	return &agent.Event{
		Type:            agent.SubagentStart,
		SessionID:       raw.ConversationID,
		SessionRef:      raw.TranscriptPath,
		SubagentID:      raw.SubagentID,
		ToolUseID:       raw.SubagentID,
		SubagentType:    raw.SubagentType,
		TaskDescription: raw.Task,
		Timestamp:       time.Now(),
	}, nil
}

func (c *CursorAgent) parseSubagentStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[subagentStopHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	if raw.Task == "" {
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	}
	event := &agent.Event{
		Type:            agent.SubagentEnd,
		SessionID:       raw.ConversationID,
		SessionRef:      raw.TranscriptPath,
		ToolUseID:       raw.SubagentID,
		SubagentType:    raw.SubagentType,
		TaskDescription: raw.Task,
		Timestamp:       time.Now(),
		SubagentID:      raw.SubagentID,
		ModifiedFiles:   raw.ModifiedFiles,
	}
	return event, nil
}
