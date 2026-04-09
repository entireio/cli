package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Compile-time interface assertions for new interfaces.
var (
	_ agent.TranscriptAnalyzer     = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptPreparer     = (*ClaudeCodeAgent)(nil)
	_ agent.TokenCalculator        = (*ClaudeCodeAgent)(nil)
	_ agent.SubagentAwareExtractor = (*ClaudeCodeAgent)(nil)
	_ agent.HookResponseWriter     = (*ClaudeCodeAgent)(nil)
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
	}
}

// ParseHookEvent translates a Claude Code hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (c *ClaudeCodeAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return c.parseSessionStart(stdin)
	case HookNameUserPromptSubmit:
		return c.parseTurnStart(stdin)
	case HookNameStop:
		return c.parseTurnEnd(stdin)
	case HookNameSessionEnd:
		return c.parseSessionEnd(stdin)
	case HookNamePreTask:
		return c.parseSubagentStart(stdin)
	case HookNamePostTask:
		return c.parseSubagentEnd(stdin)
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

func (c *ClaudeCodeAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionStart,
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
		Prompt:     raw.Prompt,
		Timestamp:  time.Now(),
	}, nil
}

func (c *ClaudeCodeAgent) parseTurnEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (c *ClaudeCodeAgent) parseSessionEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      raw.Model,
		Timestamp:  time.Now(),
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
	}
	if raw.ToolResponse.AgentID != "" {
		event.SubagentID = raw.ToolResponse.AgentID
	}
	return event, nil
}

// --- Transcript flush sentinel ---

// stopHookSentinel is the string that appears in Claude Code's hook_progress
// entry when the stop hook has been invoked, indicating the transcript is fully flushed.
const stopHookSentinel = "hooks claude-code stop"

// waitForTranscriptFlush polls the transcript file for the stop hook sentinel.
// Falls back silently after a timeout.
func waitForTranscriptFlush(ctx context.Context, transcriptPath string, hookStartTime time.Time) {
	const (
		maxWait      = 3 * time.Second
		pollInterval = 50 * time.Millisecond
		tailBytes    = 4096
		maxSkew      = 2 * time.Second
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
	for time.Now().Before(deadline) {
		if checkStopSentinel(transcriptPath, tailBytes, hookStartTime, maxSkew) {
			logging.Debug(logCtx, "transcript flush sentinel found",
				slog.Duration("wait", time.Since(hookStartTime)),
			)
			return
		}
		time.Sleep(pollInterval)
	}
	logging.Warn(logCtx, "transcript flush sentinel not found within timeout, proceeding",
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
