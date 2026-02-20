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
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface assertions for new interfaces.
var (
	_ agent.TranscriptAnalyzer     = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptPreparer     = (*ClaudeCodeAgent)(nil)
	_ agent.TokenCalculator        = (*ClaudeCodeAgent)(nil)
	_ agent.SubagentAwareExtractor = (*ClaudeCodeAgent)(nil)
)

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
func (c *ClaudeCodeAgent) ParseHookEvent(hookName string, stdin io.Reader) (*agent.Event, error) {
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

// ExtractPrompts extracts user prompts from the transcript starting at the given line offset.
func (c *ClaudeCodeAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	lines, _, err := transcript.ParseFromFileAtLine(sessionRef, fromOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	var prompts []string
	for i := range lines {
		if lines[i].Type != transcript.TypeUser {
			continue
		}
		content := transcript.ExtractUserContent(lines[i].Message)
		if content != "" {
			prompts = append(prompts, textutil.StripIDEContextTags(content))
		}
	}
	return prompts, nil
}

// ExtractSummary extracts the last assistant message as a session summary.
func (c *ClaudeCodeAgent) ExtractSummary(sessionRef string) (string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}

	lines, parseErr := transcript.ParseFromBytes(data)
	if parseErr != nil {
		return "", fmt.Errorf("failed to parse transcript: %w", parseErr)
	}

	// Walk backward to find last assistant text block
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Type != transcript.TypeAssistant {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(lines[i].Message, &msg); err != nil {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == transcript.ContentTypeText && block.Text != "" {
				return block.Text, nil
			}
		}
	}
	return "", nil
}

// PrepareTranscript waits for Claude Code's async transcript flush to complete.
// Claude writes a hook_progress sentinel entry after flushing all pending writes.
func (c *ClaudeCodeAgent) PrepareTranscript(sessionRef string) error {
	waitForTranscriptFlush(sessionRef, time.Now())
	return nil
}

// CalculateTokenUsage computes token usage from the transcript starting at the given line offset.
func (c *ClaudeCodeAgent) CalculateTokenUsage(sessionRef string, fromOffset int) (*agent.TokenUsage, error) {
	// Subagent transcripts live in <transcriptDir>/<sessionID>/subagents/
	// but we don't have the sessionID here. The caller should pass the transcript path
	// which may contain the session ID in its directory structure.
	// For now, compute subagentsDir from the transcript path structure.
	return CalculateTotalTokenUsage(sessionRef, fromOffset, "")
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
func waitForTranscriptFlush(transcriptPath string, hookStartTime time.Time) {
	const (
		maxWait      = 3 * time.Second
		pollInterval = 50 * time.Millisecond
		tailBytes    = 4096
		maxSkew      = 2 * time.Second
	)

	logCtx := logging.WithComponent(context.Background(), "agent.claudecode")
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
