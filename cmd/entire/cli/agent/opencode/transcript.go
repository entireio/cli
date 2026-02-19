package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions
var (
	_ agent.TranscriptAnalyzer = (*OpenCodeAgent)(nil)
	_ agent.TokenCalculator    = (*OpenCodeAgent)(nil)
)

// ParseTranscript parses raw JSON content into a transcript structure.
func ParseTranscript(data []byte) (*Transcript, error) {
	var t Transcript
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("failed to parse opencode transcript: %w", err)
	}
	return &t, nil
}

// parseTranscriptFile reads and parses a transcript JSON file.
func parseTranscriptFile(path string) (*Transcript, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path from agent hook
	if err != nil {
		return nil, err //nolint:wrapcheck // Callers check os.IsNotExist on this error
	}
	var t Transcript
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("failed to parse opencode transcript: %w", err)
	}
	return &t, nil
}

// GetTranscriptPosition returns the number of messages in the transcript.
func (a *OpenCodeAgent) GetTranscriptPosition(path string) (int, error) {
	t, err := parseTranscriptFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(t.Messages), nil
}

// ExtractModifiedFilesFromOffset extracts files modified by tool calls from the given message offset.
func (a *OpenCodeAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	t, err := parseTranscriptFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}

	seen := make(map[string]bool)
	var files []string

	for i := startOffset; i < len(t.Messages); i++ {
		msg := t.Messages[i]
		if msg.Role != roleAssistant {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type != "tool" || part.State == nil {
				continue
			}
			if !slices.Contains(FileModificationTools, part.Tool) {
				continue
			}
			filePath := extractFilePathFromInput(part.State.Input)
			if filePath != "" && !seen[filePath] {
				seen[filePath] = true
				files = append(files, filePath)
			}
		}
	}

	return files, len(t.Messages), nil
}

// extractFilePathFromInput extracts the file path from a tool's input map.
func extractFilePathFromInput(input map[string]interface{}) string {
	for _, key := range []string{"file_path", "path", "file", "filename"} {
		if v, ok := input[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// ExtractPrompts extracts user prompt strings from the transcript starting at the given offset.
func (a *OpenCodeAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	t, err := parseTranscriptFile(sessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var prompts []string
	for i := fromOffset; i < len(t.Messages); i++ {
		msg := t.Messages[i]
		if msg.Role == roleUser && msg.Content != "" {
			prompts = append(prompts, msg.Content)
		}
	}

	return prompts, nil
}

// ExtractSummary extracts the last assistant message content as a summary.
func (a *OpenCodeAgent) ExtractSummary(sessionRef string) (string, error) {
	t, err := parseTranscriptFile(sessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	for i := len(t.Messages) - 1; i >= 0; i-- {
		msg := t.Messages[i]
		if msg.Role == roleAssistant && msg.Content != "" {
			return msg.Content, nil
		}
	}

	return "", nil
}

// SliceFromMessage returns a JSON transcript containing only messages from startMessageIndex onward.
// This is used by explain to scope a full transcript to a specific checkpoint's portion.
func SliceFromMessage(data []byte, startMessageIndex int) []byte {
	if len(data) == 0 || startMessageIndex <= 0 {
		return data
	}

	t, err := ParseTranscript(data)
	if err != nil {
		return nil
	}

	if startMessageIndex >= len(t.Messages) {
		return nil
	}

	scoped := &Transcript{
		SessionID: t.SessionID,
		Messages:  t.Messages[startMessageIndex:],
	}

	out, err := json.Marshal(scoped)
	if err != nil {
		return nil
	}
	return out
}

// ExtractAllUserPrompts extracts all user prompts from raw transcript bytes.
// This is a package-level function used by the condensation path.
func ExtractAllUserPrompts(data []byte) ([]string, error) {
	t, err := ParseTranscript(data)
	if err != nil {
		return nil, err
	}

	var prompts []string
	for _, msg := range t.Messages {
		if msg.Role == roleUser && msg.Content != "" {
			prompts = append(prompts, msg.Content)
		}
	}
	return prompts, nil
}

// CalculateTokenUsageFromBytes computes token usage from raw transcript bytes starting at the given message offset.
// This is a package-level function used by the condensation path (which has bytes, not a file path).
func CalculateTokenUsageFromBytes(data []byte, startMessageIndex int) *agent.TokenUsage {
	t, err := ParseTranscript(data)
	if err != nil || t == nil {
		return &agent.TokenUsage{}
	}

	usage := &agent.TokenUsage{}
	for i := startMessageIndex; i < len(t.Messages); i++ {
		msg := t.Messages[i]
		if msg.Role != roleAssistant || msg.Tokens == nil {
			continue
		}
		usage.InputTokens += msg.Tokens.Input
		usage.OutputTokens += msg.Tokens.Output
		usage.CacheReadTokens += msg.Tokens.Cache.Read
		usage.CacheCreationTokens += msg.Tokens.Cache.Write
		usage.APICallCount++
	}

	return usage
}

// CalculateTokenUsage computes token usage from assistant messages starting at the given offset.
func (a *OpenCodeAgent) CalculateTokenUsage(sessionRef string, fromOffset int) (*agent.TokenUsage, error) {
	t, err := parseTranscriptFile(sessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // nil usage for nonexistent file is expected
		}
		return nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	usage := &agent.TokenUsage{}
	for i := fromOffset; i < len(t.Messages); i++ {
		msg := t.Messages[i]
		if msg.Role != roleAssistant || msg.Tokens == nil {
			continue
		}
		usage.InputTokens += msg.Tokens.Input
		usage.OutputTokens += msg.Tokens.Output
		usage.CacheReadTokens += msg.Tokens.Cache.Read
		usage.CacheCreationTokens += msg.Tokens.Cache.Write
		usage.APICallCount++
	}

	return usage, nil
}
