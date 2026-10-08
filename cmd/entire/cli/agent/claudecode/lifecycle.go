package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface assertions for new interfaces.
var (
	_ agent.TranscriptAnalyzer        = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptPreparer        = (*ClaudeCodeAgent)(nil)
	_ agent.TurnEndTranscriptPreparer = (*ClaudeCodeAgent)(nil)
	_ agent.TokenCalculator           = (*ClaudeCodeAgent)(nil)
	_ agent.ModelExtractor            = (*ClaudeCodeAgent)(nil)
	_ agent.SkillEventExtractor       = (*ClaudeCodeAgent)(nil)
	_ agent.SubagentAwareExtractor    = (*ClaudeCodeAgent)(nil)
	_ agent.ToolInvocationScanner     = (*ClaudeCodeAgent)(nil)
	_ agent.HookResponseWriter        = (*ClaudeCodeAgent)(nil)
	_ agent.ContextInjector           = (*ClaudeCodeAgent)(nil)
	_ agent.TaskTranscriptMatcher     = (*ClaudeCodeAgent)(nil)
)

// WriteHookResponse outputs a JSON hook response to stdout.
// Claude Code reads this JSON and displays the systemMessage to the user.
func (c *ClaudeCodeAgent) WriteHookResponse(message string) error {
	resp := struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}{SystemMessage: message}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return fmt.Errorf("failed to encode hook response: %w", err)
	}
	return nil
}

// InjectionEvent reports that Claude Code injects model context at TurnStart
// (the UserPromptSubmit hook), which supports hookSpecificOutput.additionalContext.
func (c *ClaudeCodeAgent) InjectionEvent() agent.EventType { return agent.TurnStart }

// RenderContextInjection renders the UserPromptSubmit additionalContext payload
// Claude Code injects into the model context.
func (c *ClaudeCodeAgent) RenderContextInjection(inj agent.ContextInjection) ([]byte, error) {
	out, err := agent.RenderAdditionalContextHookOutput("UserPromptSubmit", inj.Text)
	if err != nil {
		return nil, fmt.Errorf("render claude-code context injection: %w", err)
	}
	return out, nil
}

// HookNames returns the hook verbs Claude Code supports.
// These become subcommands: entire hooks claude-code <verb>
func (c *ClaudeCodeAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameStopFailure,
		HookNameUserPromptSubmit,
		HookNamePreTask,
		HookNamePostTask,
		HookNamePostTodo,
		HookNameSubagentStop,
	}
}

// ParseHookEvent translates a Claude Code hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (c *ClaudeCodeAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionInfoEvent(stdin, agent.SessionStart)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(stdin)
	case HookNameStop:
		return c.parseSessionInfoEvent(stdin, agent.TurnEnd)
	case HookNameStopFailure:
		// StopFailure fires instead of Stop when a turn ends on an API error
		// (rate limit, overload, auth, max output tokens, ...). The turn is over
		// and Claude Code waits for the next prompt, so it ends the turn like
		// Stop; without it the session stays ACTIVE until the next prompt.
		return c.parseSessionInfoEvent(stdin, agent.TurnEnd)
	case HookNameSessionEnd:
		return c.parseSessionInfoEvent(stdin, agent.SessionEnd)
	case HookNamePreTask:
		return c.parseSubagentStart(stdin)
	case HookNamePostTask:
		return c.parseSubagentEnd(stdin)
	case HookNameSubagentStop:
		return c.parseSubagentStop(ctx, stdin)
	case HookNamePostTodo:
		// PostTodo is Claude-specific; handled outside the generic dispatcher.
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (c *ClaudeCodeAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// PrepareTranscript waits for Claude Code's async transcript writes to settle.
// Outside the Stop hook there is no final message to wait for, so it relies on
// the file size settling; see waitForTranscriptFlush.
func (c *ClaudeCodeAgent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	waitForTranscriptFlush(ctx, sessionRef, time.Now(), "")
	return nil
}

// PrepareTurnEndTranscript waits like PrepareTranscript, but returns as soon as
// the turn's final assistant message (from the Stop payload) is on disk.
func (c *ClaudeCodeAgent) PrepareTurnEndTranscript(ctx context.Context, event *agent.Event) error {
	waitForTranscriptFlush(ctx, event.SessionRef, time.Now(), event.FinalAssistantText)
	return nil
}

// CalculateTokenUsage computes token usage from the transcript starting at the given line offset.
func (c *ClaudeCodeAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	return c.CalculateTotalTokenUsage(transcriptData, fromOffset, "")
}

// --- Internal hook parsing functions ---

// parseSessionInfoEvent parses the hooks whose payload is sessionInfoRaw —
// SessionStart, Stop, StopFailure, and SessionEnd differ only in the resulting event type.
func (c *ClaudeCodeAgent) parseSessionInfoEvent(stdin io.Reader, eventType agent.EventType) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	var finalText string
	if len(raw.LastAssistantMessage) > 0 {
		//nolint:errcheck // a non-string payload just leaves the wait on its size fallback
		_ = json.Unmarshal(raw.LastAssistantMessage, &finalText)
	}
	return &agent.Event{
		Type:               eventType,
		SessionID:          raw.SessionID,
		SessionRef:         raw.TranscriptPath,
		Model:              raw.Model,
		FinalAssistantText: finalText,
		Timestamp:          time.Now(),
	}, nil
}

func (c *ClaudeCodeAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		// Strip IDE-injected context (e.g. <ide_opened_file> from the VS Code
		// extension) so the session/checkpoint title and prompt show what the
		// user actually typed, not the injected block.
		Prompt:    textutil.StripIDEContextTags(raw.Prompt),
		Timestamp: time.Now(),
	}, nil
}

func (c *ClaudeCodeAgent) parseSubagentStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[taskHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SubagentStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		ToolUseID:  raw.ToolUseID,
		ToolInput:  raw.ToolInput,
		Timestamp:  time.Now(),
	}, nil
}

func (c *ClaudeCodeAgent) parseSubagentEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[postToolHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	event := &agent.Event{
		Type:       agent.SubagentEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		ToolUseID:  raw.ToolUseID,
		ToolInput:  raw.ToolInput,
		Timestamp:  time.Now(),
		// Final stays false: PostToolUse fires at the background launch stub,
		// seconds after launch, not at true completion. SubagentStop
		// (parseSubagentStop) is the true-completion signal.
		Final:          false,
		SubagentLaunch: subagentLaunchMode(raw.ToolResponse.Status, raw.ToolResponse.IsAsync),
	}
	if raw.ToolResponse.AgentID != "" {
		event.SubagentID = raw.ToolResponse.AgentID
	}
	return event, nil
}

// subagentLaunchMode classifies an Agent call from its tool_response. Claude
// Code decides whether a subagent runs in the background, often without the
// model passing run_in_background at all, so the response is authoritative.
func subagentLaunchMode(status string, isAsync bool) agent.SubagentLaunchMode {
	switch {
	case isAsync || status == agentToolStatusAsyncLaunched:
		return agent.SubagentLaunchBackground
	case status == agentToolStatusCompleted:
		return agent.SubagentLaunchForeground
	default:
		return agent.SubagentLaunchUnknown
	}
}

// parseSubagentStop parses Claude Code's SubagentStop hook, the true
// completion signal for a subagent — including background subagents, which
// finish long after the launch-time PostToolUse (post-task) stub fires. It
// translates into the same agent.SubagentEnd event parseSubagentEnd produces,
// but marked Final so downstream lifecycle code captures now rather than
// deferring, and carrying the subagent's own transcript path directly from
// the payload (authoritative) instead of leaving it to be resolved.
func (c *ClaudeCodeAgent) parseSubagentStop(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	rawBytes, err := agent.ReadHookInputRaw(stdin)
	if err != nil {
		return nil, fmt.Errorf("read hook input: %w", err)
	}

	// Debug log of the RAW payload's key names (never values), not the parsed
	// struct's non-empty fields: a parsed-struct view can't tell key-absent
	// from key-present-but-empty, and can't reveal an alternate key spelling
	// in the real settings-file payload. Removable once real-payload key sets
	// have been observed and the parse below is confirmed against them.
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(rawBytes, &rawMap); err == nil {
		keys := make([]string, 0, len(rawMap))
		for k := range rawMap {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		logCtx := logging.WithComponent(ctx, "agent.claudecode")
		logging.Debug(logCtx, "subagent-stop payload keys present",
			slog.Any("keys", keys),
		)
	}

	var raw subagentStopHookInputRaw
	if err := json.Unmarshal(rawBytes, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	// Tripwire: a well-formed SubagentStop payload always carries session_id.
	// tool_use_id is not checked: Claude Code's SubagentStop does not send it
	// (observed through 2.1.288), so the lifecycle correlates on agent_id.
	if raw.SessionID == "" {
		logging.Warn(logging.WithComponent(ctx, "agent.claudecode"),
			"subagent-stop payload missing session_id")
	}

	return &agent.Event{
		Type:                   agent.SubagentEnd,
		SessionID:              raw.SessionID,
		SessionRef:             raw.TranscriptPath,
		ToolUseID:              raw.ToolUseID,
		SubagentID:             raw.AgentID,
		SubagentTranscriptPath: raw.AgentTranscriptPath,
		Final:                  true,
		Timestamp:              time.Now(),
	}, nil
}

// --- Transcript flush wait ---

// waitForTranscriptFlush waits until Claude Code's async transcript writes have
// settled before turn-end reads the file. It returns as soon as EITHER the
// turn's final assistant message is on disk OR the file size has held steady
// for a full quiet window, and gives up after maxWait as a safety bound.
//
// The final-message check is the completion signal. Claude Code's Stop payload
// carries the turn's last assistant text (last_assistant_message), and the
// transcript entry holding it is written just before Stop hooks run, so it is
// usually already there on the first poll. Claude Code documents that the
// transcript may lag the in-memory conversation when hooks fire, which is why
// the check polls rather than reads once. finalText is empty when the payload
// has no final message (StopFailure, or callers outside the Stop hook).
//
// Settle-on-stability is the fallback for those cases and for a final entry the
// check cannot find. It is only a heuristic proxy for completion, so we require
// the size to hold steady across a wall-clock quietWindow (not just a poll or
// two) before trusting it. A shorter window risks a brief mid-write pause — a GC
// pause, disk contention, or a large tool-result flushed as several writes —
// being mistaken for a finished transcript, causing turn-end to read a TRUNCATED
// transcript that then gets condensed and pushed. Any observed growth resets the
// window, so a transcript still being written with sub-second pauses keeps
// waiting up to maxWait, while a genuinely settled file still returns well under it.
//
// Claude Code once wrote a hook_progress entry naming the stop hook when it
// launched, and this wait used it as its signal. Current releases record hooks
// only after they finish (stop_hook_summary), which a wait inside the hook can
// never see, so that check was removed.
func waitForTranscriptFlush(ctx context.Context, transcriptPath string, hookStartTime time.Time, finalText string) {
	const (
		maxWait      = 3 * time.Second
		pollInterval = 50 * time.Millisecond
		// quietWindow is how long the transcript size must hold steady before
		// settle-on-stability is trusted. It must comfortably exceed a plausible
		// mid-write pause so a brief stall is not mistaken for completion, while
		// still returning well under maxWait on a genuinely settled file.
		quietWindow = 500 * time.Millisecond
	)

	logCtx := logging.WithComponent(ctx, "agent.claudecode")

	// Fast path: skip the poll loop when nothing more can arrive.
	// - File doesn't exist: nothing to poll.
	// - File is stale (unmodified for 2+ min): agent isn't running anymore.
	//   This avoids 3s timeouts per stale "active" session (e.g., agent crashed
	//   without firing stop hook).
	const staleThreshold = 2 * time.Minute
	info, err := os.Stat(transcriptPath)
	if err != nil {
		// Most likely the file doesn't exist; other errors (permission, etc.)
		// would also prevent polling, so skip the wait either way.
		return
	}
	fileAge := time.Since(info.ModTime())
	if fileAge > staleThreshold {
		logging.Debug(logCtx, "transcript file is stale, skipping flush wait",
			slog.Duration("file_age", fileAge),
		)
		return
	}

	deadline := time.Now().Add(maxWait)
	lastSize := int64(-1)
	var stableSince time.Time
	for time.Now().Before(deadline) {
		if finalText != "" && finalMessageWritten(transcriptPath, finalText) {
			logging.Debug(logCtx, "transcript holds the final assistant message, proceeding",
				slog.Duration("wait", time.Since(hookStartTime)),
			)
			return
		}

		// Settle-on-stability fallback: trust the file only once its size has held
		// steady for the full quietWindow. Any growth resets the window, so a
		// sub-second pause mid-write keeps us waiting rather than returning on a
		// truncated transcript.
		if fi, statErr := os.Stat(transcriptPath); statErr == nil {
			switch {
			case fi.Size() != lastSize:
				lastSize = fi.Size()
				stableSince = time.Now()
			case time.Since(stableSince) >= quietWindow:
				logging.Debug(logCtx, "transcript settled (size stable through quiet window), proceeding",
					slog.Duration("wait", time.Since(hookStartTime)),
					slog.Duration("quiet_window", quietWindow),
					slog.Int64("size", fi.Size()),
					slog.Bool("had_final_text", finalText != ""),
				)
				return
			}
		}

		time.Sleep(pollInterval)
	}
	logging.Warn(logCtx, "transcript flush not settled within timeout, proceeding",
		slog.Duration("timeout", maxWait),
	)
}

// finalMessageTailBytes bounds how much of the transcript finalMessageWritten
// reads. Final assistant entries are a few KB; one larger than this is simply
// not found, and the wait falls back to size stability.
const finalMessageTailBytes = 256 << 10

// finalMessageWritten reports whether the transcript's tail holds the turn's
// final assistant message: the latest end_turn text block ends finalText (all
// of it, or its last block when the message has several), with no user entry
// and no entry of a later assistant message after it. Any user entry — a prompt or a tool result — means a later
// step followed, so an earlier turn that ended with the same words never
// matches.
func finalMessageWritten(path, finalText string) bool {
	f, err := os.Open(path) //nolint:gosec // path comes from agent hook input
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false
	}
	offset := max(info.Size()-finalMessageTailBytes, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return false
	}

	want := strings.TrimSpace(finalText)
	found := false
	matchedID := ""
	for _, line := range strings.Split(string(buf), "\n") {
		var entry transcript.Line
		// A partial first line (cut by the tail window) or a line still being
		// written fails to parse and is skipped.
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		switch entry.Type {
		case transcript.TypeUser:
			found = false
		case transcript.TypeAssistant:
			var msg struct {
				ID         string                    `json:"id"`
				StopReason string                    `json:"stop_reason"`
				Content    []transcript.ContentBlock `json:"content"`
			}
			if json.Unmarshal(entry.Message, &msg) != nil {
				continue
			}
			last := ""
			for _, block := range msg.Content {
				if text := strings.TrimSpace(block.Text); block.Type == transcript.ContentTypeText && text != "" {
					last = text
				}
			}
			switch {
			case msg.StopReason == "end_turn" && last != "":
				// The latest final text block decides: an earlier block that
				// happens to match must not stand once a later one is written.
				found = strings.HasSuffix(want, last)
				matchedID = msg.ID
			case msg.ID == "" || msg.ID != matchedID:
				// Any entry of a later message (thinking, tool use) means the
				// matched one was not the turn's last. Further blocks of the
				// matched message itself keep the match.
				found = false
			}
		}
	}
	return found
}
