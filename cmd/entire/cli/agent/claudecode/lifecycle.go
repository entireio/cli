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
)

// Compile-time interface assertions for new interfaces.
var (
	_ agent.TranscriptAnalyzer     = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptPreparer     = (*ClaudeCodeAgent)(nil)
	_ agent.TokenCalculator        = (*ClaudeCodeAgent)(nil)
	_ agent.ModelExtractor         = (*ClaudeCodeAgent)(nil)
	_ agent.SkillEventExtractor    = (*ClaudeCodeAgent)(nil)
	_ agent.SubagentAwareExtractor = (*ClaudeCodeAgent)(nil)
	_ agent.ToolInvocationScanner  = (*ClaudeCodeAgent)(nil)
	_ agent.HookResponseWriter     = (*ClaudeCodeAgent)(nil)
	_ agent.ContextInjector        = (*ClaudeCodeAgent)(nil)
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
	event, err := c.parseHookEvent(ctx, hookName, stdin)
	if err != nil || event == nil {
		return event, err
	}
	// Claude Code always sends session_id. An event without one did not come
	// from Claude Code, and there is nothing valid to record for it.
	//
	// This is not hypothetical: other agents read Claude Code's config. Grok
	// Build scans ~/.claude/settings.json for hooks by default and treats the
	// user scope as always-trusted, so on a machine with both installed it
	// executes Entire's `entire hooks claude-code ...` commands and feeds them
	// its own payloads — which spell the field sessionId, not session_id.
	//
	// Scope of this guard, measured rather than assumed: firing real captured
	// Grok payloads at these hooks in an Entire-enabled repo created no session
	// and no state either side of this check, so the downstream path was
	// already inert. This makes that outcome explicit and, more usefully, warns
	// — otherwise the only symptom of another agent driving our hooks is silence.
	// It is hardening plus a diagnostic, not a fix for observed corruption. The
	// actual fix is to stop the foreign invocation at its source:
	// `[compat.claude] hooks = false` in ~/.grok/config.toml.
	if event.SessionID == "" {
		logging.Warn(ctx, "claude-code: hook payload has no session_id; ignoring",
			"hook", hookName,
			"hint", "another agent may be invoking Claude Code's hooks; see docs/architecture/agent-guide.md")
		return nil, nil //nolint:nilnil // unidentifiable payload = no lifecycle action
	}
	return event, nil
}

func (c *ClaudeCodeAgent) parseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionInfoEvent(stdin, agent.SessionStart)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(stdin)
	case HookNameStop:
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

// PrepareTranscript waits for Claude Code's async transcript flush to complete.
// Claude writes a hook_progress sentinel entry after flushing all pending writes.
func (c *ClaudeCodeAgent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	waitForTranscriptFlush(ctx, sessionRef, time.Now())
	return nil
}

// CalculateTokenUsage computes token usage from the transcript starting at the given line offset.
func (c *ClaudeCodeAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	return c.CalculateTotalTokenUsage(transcriptData, fromOffset, "")
}

// --- Internal hook parsing functions ---

// parseSessionInfoEvent parses the hooks whose payload is sessionInfoRaw —
// SessionStart, Stop, and SessionEnd differ only in the resulting event type.
func (c *ClaudeCodeAgent) parseSessionInfoEvent(stdin io.Reader, eventType agent.EventType) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       eventType,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      raw.Model,
		Timestamp:  time.Now(),
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
		Final: false,
	}
	if raw.ToolResponse.AgentID != "" {
		event.SubagentID = raw.ToolResponse.AgentID
	}
	return event, nil
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

	// Tripwire for the defensive-parse assumption: a well-formed SubagentStop
	// payload always carries these two fields, so an empty value here means
	// the payload shape diverged from what this parse expects (e.g. an
	// alternate key spelling — see the key-name log above).
	if raw.ToolUseID == "" || raw.SessionID == "" {
		logging.Warn(logging.WithComponent(ctx, "agent.claudecode"),
			"subagent-stop payload missing tool_use_id or session_id — structurally impossible for a well-formed payload",
			slog.Bool("has_tool_use_id", raw.ToolUseID != ""),
			slog.Bool("has_session_id", raw.SessionID != ""))
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

// --- Transcript flush sentinel ---

// stopHookSentinel is the string that appears in Claude Code's hook_progress
// entry when the stop hook has been invoked, indicating the transcript is fully flushed.
const stopHookSentinel = "hooks claude-code stop"

// waitForTranscriptFlush waits until Claude Code's async transcript writes have
// settled before turn-end reads the file. It returns as soon as EITHER the stop
// hook sentinel appears OR the file size has held steady for a full quiet
// window, and gives up after maxWait as a safety bound.
//
// The stop-hook sentinel ("hooks claude-code stop" hook_progress entry) is the
// authoritative completion signal and the primary fast-path — when present it
// means the transcript is fully flushed and we return at once. But it is not
// reliably present while this hook runs: Claude persists it around the hook
// boundary, so a poll loop inside the stop hook often never observes it and
// would otherwise burn the full maxWait on every healthy turn-end.
//
// Settle-on-stability is therefore the fallback. It is only a heuristic proxy
// for completion, not a completion signal, so we require the size to hold steady
// across a wall-clock quietWindow (not just a poll or two) before trusting it.
// A shorter window risks a brief mid-write pause — a GC pause, disk contention,
// or a large tool-result flushed as several writes — being mistaken for a
// finished transcript, causing turn-end to read a TRUNCATED transcript that then
// gets condensed and pushed. Any observed growth resets the window, so a
// transcript still being written with sub-second pauses keeps waiting up to
// maxWait, while a genuinely settled file still returns well under it.
func waitForTranscriptFlush(ctx context.Context, transcriptPath string, hookStartTime time.Time) {
	const (
		maxWait      = 3 * time.Second
		pollInterval = 50 * time.Millisecond
		tailBytes    = 4096
		maxSkew      = 2 * time.Second
		// quietWindow is how long the transcript size must hold steady before
		// settle-on-stability is trusted. It must comfortably exceed a plausible
		// mid-write pause so a brief stall is not mistaken for completion, while
		// still returning well under maxWait on a genuinely settled file.
		quietWindow = 500 * time.Millisecond
	)

	logCtx := logging.WithComponent(ctx, "agent.claudecode")

	// Fast path: skip the poll loop when the sentinel can't possibly appear.
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
		logging.Debug(logCtx, "transcript file is stale, skipping sentinel wait",
			slog.Duration("file_age", fileAge),
		)
		return
	}

	deadline := time.Now().Add(maxWait)
	lastSize := int64(-1)
	var stableSince time.Time
	for time.Now().Before(deadline) {
		// Authoritative fast-path: the stop-hook sentinel means the transcript is
		// fully flushed, so return immediately without waiting out the window.
		if checkStopSentinel(transcriptPath, tailBytes, hookStartTime, maxSkew) {
			logging.Debug(logCtx, "transcript flush sentinel found",
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

// checkStopSentinel reads the tail of the transcript file and looks for the sentinel.
func checkStopSentinel(path string, tailBytes int64, hookStartTime time.Time, maxSkew time.Duration) bool {
	f, err := os.Open(path) //nolint:gosec // path comes from agent hook input
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false
	}
	offset := info.Size() - tailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return false
	}

	lines := strings.Split(string(buf), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, stopHookSentinel) {
			continue
		}

		var entry struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			ts, err = time.Parse(time.RFC3339, entry.Timestamp)
			if err != nil {
				continue
			}
		}
		// Validate timestamp is within acceptable range:
		// - Not too far in the past (before hook started minus skew)
		// - Not too far in the future (after hook started plus skew)
		lowerBound := hookStartTime.Add(-maxSkew)
		upperBound := hookStartTime.Add(maxSkew)
		if ts.After(lowerBound) && ts.Before(upperBound) {
			return true
		}
	}
	return false
}
