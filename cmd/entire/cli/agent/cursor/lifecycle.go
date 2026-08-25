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
		return c.parseSessionStart(ctx, stdin)
	case HookNameBeforeSubmitPrompt:
		return c.parseTurnStart(ctx, stdin)
	case HookNameStop:
		return c.parseTurnEnd(ctx, stdin)
	case HookNameSessionEnd:
		return c.parseSessionEnd(ctx, stdin)
	case HookNamePreCompact:
		return c.parsePreCompact(ctx, stdin)
	case HookNameSubagentStart:
		return c.parseSubagentStart(ctx, stdin)
	case HookNameSubagentStop:
		return c.parseSubagentStop(ctx, stdin)
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

const cursorTranscriptPathEnv = "CURSOR_TRANSCRIPT_PATH"

// resolveTranscriptRef returns the transcript path from the hook input, then
// from Cursor's documented hook environment, or computes it dynamically as the
// final fallback (Cursor CLI / IDE layout).
//
// Cursor Cloud and CLI often send transcript_path:null. When a local file
// exists, Cursor may also set CURSOR_TRANSCRIPT_PATH for command hooks — prefer
// that over fabricating a path. On Cloud Agents the local
// ~/.cursor/projects/.../agent-transcripts directory is never created
// (conversation history is remote-only); we still return the computed path so
// session-agent identity matching keeps working, while PrepareTranscript skips
// its flush wait when the parent directory is absent.
func (c *CursorAgent) resolveTranscriptRef(ctx context.Context, conversationID, rawPath string) string {
	if rawPath != "" {
		return rawPath
	}
	if envPath := os.Getenv(cursorTranscriptPathEnv); envPath != "" {
		return envPath
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

func (c *CursorAgent) parseSessionStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionStartRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  raw.ConversationID,
		SessionRef: c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
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
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.ConversationID,
		SessionRef: c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		Model:      raw.Model,
		TurnCount:  int(intFromJSON(raw.LoopCount)),
		Timestamp:  time.Now(),
	}
	event.TokenUsage = tokenUsageFromStop(raw)
	return event, nil
}

// tokenUsageFromStop converts the per-turn token fields in Cursor's stop hook
// payload into the framework-wide TokenUsage struct. Cursor reports
// input_tokens as the *total* input (cache_read + cache_write + fresh), so we
// derive the fresh-input portion here. Returns nil when no usable token fields
// are present (some Cursor versions / hook variants omit them entirely),
// signaling "no data" rather than "all zeros".
func tokenUsageFromStop(raw *stopHookInputRaw) *agent.TokenUsage {
	totalInput := int(intFromJSON(raw.InputTokens))
	output := int(intFromJSON(raw.OutputTokens))
	if totalInput == 0 && output == 0 {
		return nil
	}
	cacheRead := int(intFromJSON(raw.CacheReadTokens))
	cacheWrite := int(intFromJSON(raw.CacheWriteTokens))
	freshInput := totalInput - cacheRead - cacheWrite
	if freshInput < 0 {
		freshInput = 0
	}
	return &agent.TokenUsage{
		InputTokens:         freshInput,
		CacheCreationTokens: cacheWrite,
		CacheReadTokens:     cacheRead,
		OutputTokens:        output,
		APICallCount:        1,
	}
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

func (c *CursorAgent) parsePreCompact(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[preCompactHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:              agent.Compaction,
		SessionID:         raw.ConversationID,
		SessionRef:        c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		ContextTokens:     int(intFromJSON(raw.ContextTokens)),
		ContextWindowSize: int(intFromJSON(raw.ContextWindowSize)),
		Timestamp:         time.Now(),
	}, nil
}

func (c *CursorAgent) parseSubagentStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
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
		SessionRef:      c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		SubagentID:      raw.SubagentID,
		ToolUseID:       raw.SubagentID,
		SubagentType:    raw.SubagentType,
		TaskDescription: raw.Task,
		Timestamp:       time.Now(),
	}, nil
}

func (c *CursorAgent) parseSubagentStop(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
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
		SessionRef:      c.resolveTranscriptRef(ctx, raw.ConversationID, raw.TranscriptPath),
		ToolUseID:       raw.SubagentID,
		SubagentType:    raw.SubagentType,
		TaskDescription: raw.Task,
		Timestamp:       time.Now(),
		SubagentID:      raw.SubagentID,
		ModifiedFiles:   raw.ModifiedFiles,
		// Cursor names the subagent's transcript in the payload. This field was
		// parsed but never forwarded, so the framework fell back to guessing Claude
		// Code's layout — which does not exist under Cursor's session dir, so the
		// task checkpoint stored no subagent transcript at all.
		SubagentTranscriptPath: raw.AgentTranscriptPath,
	}
	return event, nil
}
