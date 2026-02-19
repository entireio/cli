package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions
var (
	_ agent.TranscriptAnalyzer = (*OpenCodeAgent)(nil)
	_ agent.TokenCalculator    = (*OpenCodeAgent)(nil)
)

// ParseMessages parses JSONL content (one Message per line) into a slice of Messages.
func ParseMessages(data []byte) ([]Message, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var messages []Message
	reader := bufio.NewReader(bytes.NewReader(data))

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read opencode transcript: %w", err)
		}

		if len(bytes.TrimSpace(lineBytes)) > 0 {
			var msg Message
			if jsonErr := json.Unmarshal(lineBytes, &msg); jsonErr == nil {
				messages = append(messages, msg)
			}
		}

		if err == io.EOF {
			break
		}
	}

	return messages, nil
}

// parseMessagesFromFile reads and parses a JSONL transcript file.
func parseMessagesFromFile(path string) ([]Message, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path from agent hook
	if err != nil {
		return nil, err //nolint:wrapcheck // Callers check os.IsNotExist on this error
	}
	return ParseMessages(data)
}

// GetTranscriptPosition returns the number of JSONL lines in the transcript.
func (a *OpenCodeAgent) GetTranscriptPosition(path string) (int, error) {
	messages, err := parseMessagesFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(messages), nil
}

// ExtractModifiedFilesFromOffset extracts files modified by tool calls from the given message offset.
func (a *OpenCodeAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	messages, err := parseMessagesFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}

	seen := make(map[string]bool)
	var files []string

	for i := startOffset; i < len(messages); i++ {
		msg := messages[i]
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

	return files, len(messages), nil
}

// ExtractModifiedFiles extracts modified file paths from raw JSONL transcript bytes.
// This is the bytes-based equivalent of ExtractModifiedFilesFromOffset, used by ReadSession.
func ExtractModifiedFiles(data []byte) ([]string, error) {
	messages, err := ParseMessages(data)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var files []string

	for _, msg := range messages {
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

	return files, nil
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
	messages, err := parseMessagesFromFile(sessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var prompts []string
	for i := fromOffset; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == roleUser && msg.Content != "" {
			prompts = append(prompts, msg.Content)
		}
	}

	return prompts, nil
}

// ExtractSummary extracts the last assistant message content as a summary.
func (a *OpenCodeAgent) ExtractSummary(sessionRef string) (string, error) {
	messages, err := parseMessagesFromFile(sessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == roleAssistant && msg.Content != "" {
			return msg.Content, nil
		}
	}

	return "", nil
}

// ExtractAllUserPrompts extracts all user prompts from raw JSONL transcript bytes.
// This is a package-level function used by the condensation path.
func ExtractAllUserPrompts(data []byte) ([]string, error) {
	messages, err := ParseMessages(data)
	if err != nil {
		return nil, err
	}

	var prompts []string
	for _, msg := range messages {
		if msg.Role == roleUser && msg.Content != "" {
			prompts = append(prompts, msg.Content)
		}
	}
	return prompts, nil
}

// CalculateTokenUsageFromBytes computes token usage from raw JSONL transcript bytes
// starting at the given message offset.
// This is a package-level function used by the condensation path (which has bytes, not a file path).
func CalculateTokenUsageFromBytes(data []byte, startMessageIndex int) *agent.TokenUsage {
	messages, err := ParseMessages(data)
	if err != nil || messages == nil {
		return &agent.TokenUsage{}
	}

	usage := &agent.TokenUsage{}
	for i := startMessageIndex; i < len(messages); i++ {
		msg := messages[i]
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
	messages, err := parseMessagesFromFile(sessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // nil usage for nonexistent file is expected
		}
		return nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	usage := &agent.TokenUsage{}
	for i := fromOffset; i < len(messages); i++ {
		msg := messages[i]
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
