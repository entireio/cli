package geminicli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

// UnmarshalJSON handles both string and array content formats in Gemini transcripts.
// User messages use: "content": [{"text": "..."}] (array of objects)
// Gemini messages use: "content": "response text" (string)
func (m *GeminiMessage) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid infinite recursion
	type Alias GeminiMessage
	aux := &struct {
		*Alias

		Content json.RawMessage `json:"content,omitempty"`
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	if len(aux.Content) == 0 || string(aux.Content) == "null" {
		m.Content = ""
		return nil
	}

	// Try string first (most common for gemini messages)
	var strContent string
	if err := json.Unmarshal(aux.Content, &strContent); err == nil {
		m.Content = strContent
		return nil
	}

	// Try array of objects with "text" fields (user messages)
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(aux.Content, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		m.Content = strings.Join(texts, "\n")
		return nil
	}

	// Unknown format - leave content empty
	return nil
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

// NormalizeTranscript normalizes user message content fields in-place from
// [{"text":"..."}] arrays to plain strings, preserving all other transcript fields
// (timestamps, thoughts, tokens, model, toolCalls, etc.).
//
// This operates on raw JSON rather than using ParseTranscript + re-marshal because
// GeminiMessage only captures a subset of fields (id, type, content, toolCalls).
// Round-tripping through the struct would silently drop fields like timestamp, model,
// and tokens that are present in real Gemini transcripts. The raw approach rewrites
// only the content values while leaving all other fields untouched.
func NormalizeTranscript(data []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	messagesRaw, ok := raw["messages"]
	if !ok {
		return data, nil
	}

	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, fmt.Errorf("failed to parse messages: %w", err)
	}

	changed := false
	for i, msgRaw := range messages {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			continue
		}

		contentRaw, hasContent := msg["content"]
		if !hasContent || len(contentRaw) == 0 {
			continue
		}

		// Skip if already a string
		var strContent string
		if json.Unmarshal(contentRaw, &strContent) == nil {
			continue
		}

		// Try to convert array of {"text":"..."} to a plain string
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(contentRaw, &parts) != nil {
			continue
		}

		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		joined := strings.Join(texts, "\n")
		strBytes, err := json.Marshal(joined)
		if err != nil {
			continue
		}
		msg["content"] = strBytes
		rewritten, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		messages[i] = rewritten
		changed = true
	}

	if !changed {
		return data, nil
	}

	rewrittenMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("failed to re-serialize messages: %w", err)
	}
	raw["messages"] = rewrittenMessages

	result, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to re-serialize transcript: %w", err)
	}
	return result, nil
}

// SliceFromMessage returns a Gemini transcript scoped to messages starting from
// startMessageIndex. This is the Gemini equivalent of transcript.SliceFromLine —
// for Gemini's single JSON blob, scoping is done by message index rather than line offset.
// Returns the original data if startMessageIndex <= 0.
// Returns nil, nil if startMessageIndex exceeds the number of messages.
func SliceFromMessage(data []byte, startMessageIndex int) ([]byte, error) {
	if len(data) == 0 || startMessageIndex <= 0 {
		return data, nil
	}

	t, err := ParseTranscript(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for slicing: %w", err)
	}

	if startMessageIndex >= len(t.Messages) {
		return nil, nil
	}

	scoped := &GeminiTranscript{
		Messages: t.Messages[startMessageIndex:],
	}

	out, err := json.Marshal(scoped)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal scoped transcript: %w", err)
	}
	return out, nil
}
