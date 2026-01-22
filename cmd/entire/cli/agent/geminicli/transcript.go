package geminicli

import (
	"encoding/json"
	"fmt"
	"os"

	"entire.io/cli/cmd/entire/cli/agent"
)

// Transcript parsing types - Gemini CLI uses JSON format for session storage
// Based on transcript_path format: ~/.gemini/tmp/<hash>/chats/session-<date>-<id>.json

// Message type constants for Gemini transcripts
const (
	MessageTypeUser   = "user"
	MessageTypeGemini = "gemini"
)

// GeminiTranscript represents the top-level structure of a Gemini session file
type GeminiTranscript struct {
	Messages []GeminiMessage `json:"messages"`
}

// GeminiMessage represents a single message in the transcript
type GeminiMessage struct {
	ID        string           `json:"id,omitempty"` // UUID for the message
	Type      string           `json:"type"`         // MessageTypeUser or MessageTypeGemini
	Content   string           `json:"content,omitempty"`
	ToolCalls []GeminiToolCall `json:"toolCalls,omitempty"`
}

// GeminiToolCall represents a tool call in a gemini message
type GeminiToolCall struct {
	ID     string                 `json:"id"`
	Name   string                 `json:"name"`
	Args   map[string]interface{} `json:"args"`
	Status string                 `json:"status,omitempty"`
}

// ParseTranscript parses raw JSON content into a transcript structure
func ParseTranscript(data []byte) (*GeminiTranscript, error) {
	var transcript GeminiTranscript
	if err := json.Unmarshal(data, &transcript); err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	return &transcript, nil
}

// ExtractModifiedFiles extracts files modified by tool calls from transcript data
func ExtractModifiedFiles(data []byte) ([]string, error) {
	transcript, err := ParseTranscript(data)
	if err != nil {
		return nil, err
	}

	return ExtractModifiedFilesFromTranscript(transcript), nil
}

// ExtractModifiedFilesFromTranscript extracts files from a parsed transcript
func ExtractModifiedFilesFromTranscript(transcript *GeminiTranscript) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, msg := range transcript.Messages {
		// Only process gemini messages (assistant messages)
		if msg.Type != MessageTypeGemini {
			continue
		}

		// Process tool calls in this message
		for _, toolCall := range msg.ToolCalls {
			// Check if it's a file modification tool
			isModifyTool := false
			for _, name := range FileModificationTools {
				if toolCall.Name == name {
					isModifyTool = true
					break
				}
			}

			if !isModifyTool {
				continue
			}

			// Extract file path from args map
			var file string
			if fp, ok := toolCall.Args["file_path"].(string); ok && fp != "" {
				file = fp
			} else if p, ok := toolCall.Args["path"].(string); ok && p != "" {
				file = p
			} else if fn, ok := toolCall.Args["filename"].(string); ok && fn != "" {
				file = fn
			}

			if file != "" && !fileSet[file] {
				fileSet[file] = true
				files = append(files, file)
			}
		}
	}

	return files
}

// ExtractLastUserPrompt extracts the last user message from transcript data
func ExtractLastUserPrompt(data []byte) (string, error) {
	transcript, err := ParseTranscript(data)
	if err != nil {
		return "", err
	}

	return ExtractLastUserPromptFromTranscript(transcript), nil
}

// ExtractLastUserPromptFromTranscript extracts the last user prompt from a parsed transcript
func ExtractLastUserPromptFromTranscript(transcript *GeminiTranscript) string {
	for i := len(transcript.Messages) - 1; i >= 0; i-- {
		msg := transcript.Messages[i]
		if msg.Type != MessageTypeUser {
			continue
		}

		// Content is now a string field
		if msg.Content != "" {
			return msg.Content
		}
	}
	return ""
}

// ExtractAllUserPrompts extracts all user messages from transcript data
func ExtractAllUserPrompts(data []byte) ([]string, error) {
	transcript, err := ParseTranscript(data)
	if err != nil {
		return nil, err
	}

	return ExtractAllUserPromptsFromTranscript(transcript), nil
}

// ExtractAllUserPromptsFromTranscript extracts all user prompts from a parsed transcript
func ExtractAllUserPromptsFromTranscript(transcript *GeminiTranscript) []string {
	var prompts []string
	for _, msg := range transcript.Messages {
		if msg.Type == MessageTypeUser && msg.Content != "" {
			prompts = append(prompts, msg.Content)
		}
	}
	return prompts
}

// ExtractLastAssistantMessage extracts the last gemini response from transcript data
func ExtractLastAssistantMessage(data []byte) (string, error) {
	transcript, err := ParseTranscript(data)
	if err != nil {
		return "", err
	}

	return ExtractLastAssistantMessageFromTranscript(transcript), nil
}

// ExtractLastAssistantMessageFromTranscript extracts the last gemini response from a parsed transcript
func ExtractLastAssistantMessageFromTranscript(transcript *GeminiTranscript) string {
	for i := len(transcript.Messages) - 1; i >= 0; i-- {
		msg := transcript.Messages[i]
		if msg.Type == MessageTypeGemini && msg.Content != "" {
			return msg.Content
		}
	}
	return ""
}

// GetLastMessageID returns the ID of the last message in the transcript.
// Returns empty string if the transcript is empty or the last message has no ID.
func GetLastMessageID(data []byte) (string, error) {
	transcript, err := ParseTranscript(data)
	if err != nil {
		return "", err
	}
	return GetLastMessageIDFromTranscript(transcript), nil
}

// GetLastMessageIDFromTranscript returns the ID of the last message in a parsed transcript.
// Returns empty string if the transcript is empty or the last message has no ID.
func GetLastMessageIDFromTranscript(transcript *GeminiTranscript) string {
	if len(transcript.Messages) == 0 {
		return ""
	}
	return transcript.Messages[len(transcript.Messages)-1].ID
}

// GetLastMessageIDFromFile reads a transcript file and returns the last message's ID.
// Returns empty string if the file doesn't exist, is empty, or has no messages with IDs.
func GetLastMessageIDFromFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // Reading from controlled transcript path
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}

	if len(data) == 0 {
		return "", nil
	}

	return GetLastMessageID(data)
}
// CalculateTokenUsage calculates token usage from a Gemini transcript.
// This is specific to Gemini's API format where each message may have a tokens object
// with input, output, cached, thoughts, tool, and total counts.
// Only processes messages from startMessageIndex onwards (0-indexed).
func CalculateTokenUsage(data []byte, startMessageIndex int) *agent.TokenUsage {
	var transcript struct {
		Messages []geminiMessageWithTokens `json:"messages"`
	}

	if err := json.Unmarshal(data, &transcript); err != nil {
		return &agent.TokenUsage{}
	}

	usage := &agent.TokenUsage{}

	for i, msg := range transcript.Messages {
		// Skip messages before startMessageIndex
		if i < startMessageIndex {
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

	return usage
}

// CalculateTokenUsageFromFile calculates token usage from a Gemini transcript file.
// If startMessageIndex > 0, only considers messages from that index onwards.
func CalculateTokenUsageFromFile(path string, startMessageIndex int) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // Reading from controlled transcript path
	if err != nil {
		if os.IsNotExist(err) {
			return &agent.TokenUsage{}, nil
		}
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	return CalculateTokenUsage(data, startMessageIndex), nil
}
