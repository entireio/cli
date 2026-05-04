package geminicli

import (
	"context"
	"encoding/json"
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
	_ agent.HookResponseWriter = (*GeminiCLIAgent)(nil)
)

// WriteHookResponse outputs a hook response message as plain text to stdout.
//
// Why plain text and not JSON? Gemini CLI (as of v0.40.0) double-displays
// systemMessage when it arrives in JSON form: once via emitHookSystemMessage
// (rendered with the [hookName] source tag) and again via the SessionStart
// path's direct historyManager.addItem (rendered without a tag). With plain
// text, gemini's convertPlainTextToHookOutput synthesizes a systemMessage
// internally, the JSON-only emitHookSystemMessage event doesn't fire, and
// the user sees the banner exactly once.
func (g *GeminiCLIAgent) WriteHookResponse(message string) error {
	if message == "" {
		return nil
	}
	if _, err := fmt.Fprintln(os.Stdout, message); err != nil {
		return fmt.Errorf("failed to write hook response: %w", err)
	}
	return nil
}

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
func (g *GeminiCLIAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
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
	case HookNameBeforeModel:
		return g.parseBeforeModel(stdin)
	case HookNameBeforeTool, HookNameAfterTool,
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

// CalculateTokenUsage computes token usage from the transcript starting at the given message offset.
func (g *GeminiCLIAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	var transcript struct {
		Messages []geminiMessageWithTokens `json:"messages"`
	}

	if err := json.Unmarshal(transcriptData, &transcript); err != nil {
		return &agent.TokenUsage{}, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	usage := &agent.TokenUsage{}

	for i, msg := range transcript.Messages {
		// Skip messages before startMessageIndex
		if i < fromOffset {
			continue
		}

		// Only count tokens from gemini (assistant) messages
		if msg.Type != MessageTypeGemini {
			continue
		}

		if msg.Tokens == nil {
			continue
		}

		usage.APICallCount++
		usage.InputTokens += msg.Tokens.Input
		usage.OutputTokens += msg.Tokens.Output
		usage.CacheReadTokens += msg.Tokens.Cached
	}

	return usage, nil
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

func (g *GeminiCLIAgent) parseBeforeModel(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[beforeModelRaw](stdin)
	if err != nil {
		return nil, err
	}
	model := raw.LLMRequest.Model
	if model == "" {
		return nil, nil //nolint:nilnil // no model info → no lifecycle action
	}
	return &agent.Event{
		Type:      agent.ModelUpdate,
		SessionID: raw.SessionID,
		Model:     model,
		Timestamp: time.Now(),
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
