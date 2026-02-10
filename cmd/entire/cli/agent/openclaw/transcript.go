package openclaw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// Scanner buffer size for large transcript files (10MB)
const scannerBufferSize = 10 * 1024 * 1024

// ParseTranscript parses raw JSONL content into OpenClaw messages
func ParseTranscript(data []byte) ([]OpenClawMessage, error) {
	var messages []OpenClawMessage
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, scannerBufferSize), scannerBufferSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg OpenClawMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // Skip malformed lines
		}
		messages = append(messages, msg)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan transcript: %w", err)
	}
	return messages, nil
}

// SerializeTranscript converts messages back to JSONL bytes
func SerializeTranscript(messages []OpenClawMessage) ([]byte, error) {
	var buf bytes.Buffer
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal message: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ExtractModifiedFiles extracts file paths modified by tool calls from messages
func ExtractModifiedFiles(messages []OpenClawMessage) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}

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

			// Extract file path from params
			var file string
			if fp, ok := toolCall.Params["file_path"].(string); ok && fp != "" {
				file = fp
			} else if p, ok := toolCall.Params["path"].(string); ok && p != "" {
				file = p
			}

			if file != "" && !fileSet[file] {
				fileSet[file] = true
				files = append(files, file)
			}
		}
	}

	return files
}

// ExtractLastUserPrompt extracts the last user message from the transcript
func ExtractLastUserPrompt(messages []OpenClawMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}
