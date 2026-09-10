package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
)

// Compile-time interface assertions for new interfaces.
var (
	_ agent.TranscriptAnalyzer     = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptCapturer     = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptTurnAnalyzer = (*ClaudeCodeAgent)(nil)
	_ agent.RepeatableTurnEnd      = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptPreparer     = (*ClaudeCodeAgent)(nil)
	_ agent.TokenCalculator        = (*ClaudeCodeAgent)(nil)
	_ agent.ModelExtractor         = (*ClaudeCodeAgent)(nil)
	_ agent.SkillEventExtractor    = (*ClaudeCodeAgent)(nil)
	_ agent.SubagentAwareExtractor = (*ClaudeCodeAgent)(nil)
	_ agent.ToolInvocationScanner  = (*ClaudeCodeAgent)(nil)
	_ agent.HookResponseWriter     = (*ClaudeCodeAgent)(nil)
	_ agent.ContextInjector        = (*ClaudeCodeAgent)(nil)
)

// TurnEndMayRepeat reports Claude Code's Stop-hook continuation contract: a
// blocking Stop hook can make Claude continue and later emit another Stop
// without another UserPromptSubmit event.
func (c *ClaudeCodeAgent) TurnEndMayRepeat() bool { return true }

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

// PrepareTranscript retains best-effort readiness waiting for non-Stop call sites.
func (c *ClaudeCodeAgent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	waitForTranscriptQuiet(ctx, sessionRef)
	return nil
}

func waitForTranscriptQuiet(ctx context.Context, transcriptPath string) {
	logCtx := logging.WithComponent(ctx, "agent.claudecode")
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return
	}
	if age := time.Since(info.ModTime()); age > defaultStaleThreshold {
		logging.Debug(logCtx, "transcript file is stale, skipping readiness wait",
			slog.Duration("file_age", age))
		return
	}

	timeout := time.NewTimer(defaultMaxWait)
	defer timeout.Stop()
	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()
	lastSize := int64(-1)
	var stableSince time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout.C:
			logging.Warn(logCtx, "transcript flush not settled within timeout, proceeding",
				slog.Duration("timeout", defaultMaxWait))
			return
		case <-ticker.C:
			current, statErr := os.Stat(transcriptPath)
			if statErr != nil {
				continue
			}
			if current.Size() != lastSize {
				lastSize = current.Size()
				stableSince = time.Now()
				continue
			}
			if time.Since(stableSince) >= defaultQuietWindow {
				logging.Debug(logCtx, "transcript settled through quiet window, proceeding",
					slog.Duration("quiet_window", defaultQuietWindow),
					slog.Int64("size", current.Size()))
				return
			}
		}
	}
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
	event := &agent.Event{
		Type:       eventType,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}
	if eventType == agent.TurnEnd {
		event.FinalResponse = raw.LastAssistantMessage.value
		event.FinalResponsePresent = raw.LastAssistantMessage.present
		event.StopHookActive = raw.StopHookActive
	}
	return event, nil
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
