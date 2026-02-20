package geminicli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions for new interfaces.
var (
	_ agent.TranscriptAnalyzer = (*GeminiCLIAgent)(nil)
	_ agent.TokenCalculator    = (*GeminiCLIAgent)(nil)
)

// HookNames returns the hook verbs Gemini CLI supports.
// These become subcommands: entire hooks gemini <verb>
func (g *GeminiCLIAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameBeforeAgent,
		HookNameAfterAgent,
		HookNameBeforeModel,
		HookNameAfterModel,
		HookNameBeforeToolSelection,
		HookNameBeforeTool,
		HookNameAfterTool,
		HookNamePreCompress,
		HookNameNotification,
	}
}

// ParseHookEvent translates a Gemini CLI hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance (e.g., pass-through hooks).
func (g *GeminiCLIAgent) ParseHookEvent(hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return g.parseSessionStart(stdin)
	case HookNameBeforeAgent:
		return g.parseTurnStart(stdin)
	case HookNameAfterAgent:
		return g.parseTurnEnd(stdin)
	case HookNameSessionEnd:
		return g.parseSessionEnd(stdin)
	case HookNamePreCompress:
		return g.parseCompaction(stdin)
	case HookNameBeforeTool, HookNameAfterTool, HookNameBeforeModel,
		HookNameAfterModel, HookNameBeforeToolSelection, HookNameNotification:
		// Acknowledged hooks with no lifecycle action
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// ReadTranscript reads the raw JSON transcript bytes for a session.
func (g *GeminiCLIAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ExtractPrompts extracts user prompts from the transcript starting at the given message offset.
func (g *GeminiCLIAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	t, parseErr := ParseTranscript(data)
	if parseErr != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", parseErr)
	}

	var prompts []string
	for i := fromOffset; i < len(t.Messages); i++ {
		msg := t.Messages[i]
		if msg.Type == MessageTypeUser && msg.Content != "" {
			prompts = append(prompts, msg.Content)
		}
	}
	return prompts, nil
}

// ExtractSummary extracts the last assistant message as a session summary.
func (g *GeminiCLIAgent) ExtractSummary(sessionRef string) (string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}
	return ExtractLastAssistantMessage(data)
}

// CalculateTokenUsage computes token usage from the transcript starting at the given message offset.
func (g *GeminiCLIAgent) CalculateTokenUsage(sessionRef string, fromOffset int) (*agent.TokenUsage, error) {
	return CalculateTokenUsageFromFile(sessionRef, fromOffset)
}

// --- Internal hook parsing functions ---

func (g *GeminiCLIAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
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

func (g *GeminiCLIAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[agentHookInputRaw](stdin)
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

func (g *GeminiCLIAgent) parseTurnEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[agentHookInputRaw](stdin)
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

func (g *GeminiCLIAgent) parseSessionEnd(stdin io.Reader) (*agent.Event, error) {
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

func (g *GeminiCLIAgent) parseCompaction(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.Compaction,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}
