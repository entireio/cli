package factoryaidroid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface assertions.
var (
	_ agent.TranscriptAnalyzer     = (*FactoryAIDroidAgent)(nil)
	_ agent.TokenCalculator        = (*FactoryAIDroidAgent)(nil)
	_ agent.SubagentAwareExtractor = (*FactoryAIDroidAgent)(nil)
	_ agent.HookResponseWriter     = (*FactoryAIDroidAgent)(nil)
	_ agent.PromptExtractor        = (*FactoryAIDroidAgent)(nil)
)

// WriteHookResponse outputs the hook response as plain text to stdout.
// Factory AI Droid does not parse the JSON systemMessage protocol,
// so we write plain text that it displays directly in the terminal.
func (f *FactoryAIDroidAgent) WriteHookResponse(message string) error {
	if _, err := fmt.Fprintln(os.Stdout, message); err != nil {
		return fmt.Errorf("failed to write hook response: %w", err)
	}
	return nil
}

// HookNames returns the hook verbs Factory AI Droid supports.
// These become subcommands: entire hooks factoryai-droid <verb>
func (f *FactoryAIDroidAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNameSubagentStop,
		HookNamePreCompact,
		HookNameNotification,
	}
}

// ParseHookEvent translates a Factory AI Droid hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (f *FactoryAIDroidAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return f.parseSessionStart(stdin)
	case HookNameUserPromptSubmit:
		return f.parseTurnStart(stdin)
	case HookNameStop:
		return f.parseTurnEnd(stdin)
	case HookNameSessionEnd:
		return f.parseSessionEnd(stdin)
	case HookNamePreToolUse:
		return f.parseSubagentStart(ctx, stdin)
	case HookNamePostToolUse:
		return f.parseSubagentEnd(ctx, stdin)
	case HookNamePreCompact:
		return f.parseCompaction(stdin)
	case HookNameSubagentStop, HookNameNotification:
		// Acknowledged hooks with no lifecycle action
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// --- TranscriptAnalyzer ---

// GetTranscriptPosition returns the current line count of the JSONL transcript.
func (f *FactoryAIDroidAgent) GetTranscriptPosition(path string) (int, error) {
	_, pos, err := ParseDroidTranscript(path, 0)
	if err != nil {
		return 0, err
	}
	return pos, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line offset.
func (f *FactoryAIDroidAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	lines, currentPos, err := ParseDroidTranscript(path, startOffset)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse transcript: %w", err)
	}
	files := ExtractModifiedFiles(lines)
	return files, currentPos, nil
}

// ExtractPrompts extracts user prompts from the transcript starting at the given line offset.
func (f *FactoryAIDroidAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	lines, _, err := ParseDroidTranscript(sessionRef, fromOffset)
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
func (f *FactoryAIDroidAgent) ExtractSummary(sessionRef string) (string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}
	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		return "", fmt.Errorf("failed to parse transcript: %w", err)
	}

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

// --- TokenCalculator ---

// CalculateTokenUsage computes token usage from pre-loaded transcript bytes starting at the given line offset.
func (f *FactoryAIDroidAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	return CalculateTotalTokenUsageFromBytes(transcriptData, fromOffset, "")
}

// --- SubagentAwareExtractor ---

// ExtractAllModifiedFiles extracts files modified by both the main agent and any spawned subagents.
func (f *FactoryAIDroidAgent) ExtractAllModifiedFiles(transcriptData []byte, fromOffset int, subagentsDir string) ([]string, error) {
	return ExtractAllModifiedFilesFromBytes(transcriptData, fromOffset, subagentsDir)
}

// CalculateTotalTokenUsage computes token usage including all spawned subagents.
func (f *FactoryAIDroidAgent) CalculateTotalTokenUsage(transcriptData []byte, fromOffset int, subagentsDir string) (*agent.TokenUsage, error) {
	return CalculateTotalTokenUsageFromBytes(transcriptData, fromOffset, subagentsDir)
}

// --- Internal hook parsing functions ---

func (f *FactoryAIDroidAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
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

func (f *FactoryAIDroidAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[userPromptSubmitRaw](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Prompt:     raw.Prompt,
		Model:      raw.Model,
		Timestamp:  time.Now(),
	}, nil
}

func (f *FactoryAIDroidAgent) parseTurnEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[stopRaw](stdin)
	if err != nil {
		return nil, err
	}

	model := raw.Model
	// If model not provided in hook payload, extract from transcript
	if model == "" && raw.TranscriptPath != "" {
		model = ExtractModelFromTranscript(raw.TranscriptPath)
	}

	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Model:      model,
		Timestamp:  time.Now(),
	}, nil
}

func (f *FactoryAIDroidAgent) parseSessionEnd(stdin io.Reader) (*agent.Event, error) {
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

func (f *FactoryAIDroidAgent) parseSubagentStart(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[taskHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	toolUseID := raw.ToolUseID
	if toolUseID == "" {
		toolUseID, err = registerFallbackToolUseID(ctx, raw.SessionID, raw.ToolName, raw.ToolInput)
		if err != nil {
			return nil, err
		}
	}
	return &agent.Event{
		Type:       agent.SubagentStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		ToolUseID:  toolUseID,
		ToolInput:  raw.ToolInput,
		Timestamp:  time.Now(),
	}, nil
}

func (f *FactoryAIDroidAgent) parseSubagentEnd(ctx context.Context, stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[postToolHookInputRaw](stdin)
	if err != nil {
		return nil, err
	}
	toolUseID := raw.ToolUseID
	if toolUseID == "" {
		toolUseID, err = resolveFallbackToolUseID(ctx, raw.SessionID, raw.ToolName, raw.ToolInput)
		if err != nil {
			return nil, err
		}
	}
	event := &agent.Event{
		Type:       agent.SubagentEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		ToolUseID:  toolUseID,
		ToolInput:  raw.ToolInput,
		Timestamp:  time.Now(),
	}
	if agentID := parseHookToolResponseAgentID(raw.ToolResponse); agentID != "" {
		event.SubagentID = agentID
	}
	return event, nil
}

func (f *FactoryAIDroidAgent) parseCompaction(stdin io.Reader) (*agent.Event, error) {
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

func parseHookToolResponseAgentID(raw json.RawMessage) string {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}

	var obj struct {
		AgentID string `json:"agentId"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.AgentID != "" {
		return obj.AgentID
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return extractHookToolResponseAgentID(text)
	}

	return ""
}

func extractHookToolResponseAgentID(text string) string {
	const prefix = "agentId: "
	_, after, found := strings.Cut(text, prefix)
	if !found {
		return ""
	}

	after = strings.TrimSpace(after)
	end := 0
	for end < len(after) {
		ch := after[end]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' {
			end++
			continue
		}
		break
	}

	return after[:end]
}
