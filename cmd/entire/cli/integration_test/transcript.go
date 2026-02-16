//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TranscriptBuilder builds realistic JSONL transcripts for testing.
type TranscriptBuilder struct {
	messages       []map[string]interface{}
	toolUseCounter int
}

// NewTranscriptBuilder creates a new transcript builder.
func NewTranscriptBuilder() *TranscriptBuilder {
	return &TranscriptBuilder{
		messages: make([]map[string]interface{}, 0),
	}
}

// AddUserMessage adds a user message with string content.
func (b *TranscriptBuilder) AddUserMessage(content string) *TranscriptBuilder {
	b.messages = append(b.messages, map[string]interface{}{
		"uuid":      fmt.Sprintf("user-%d", len(b.messages)+1),
		"type":      "user",
		"message":   map[string]interface{}{"content": content},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	return b
}

// AddAssistantMessage adds an assistant message with text content.
func (b *TranscriptBuilder) AddAssistantMessage(content string) *TranscriptBuilder {
	b.messages = append(b.messages, map[string]interface{}{
		"uuid": fmt.Sprintf("asst-%d", len(b.messages)+1),
		"type": "assistant",
		"message": map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": content},
			},
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	return b
}

// AddToolUse adds a tool use (Write/Edit) to the transcript.
// Returns the tool use ID for use with AddToolResult.
func (b *TranscriptBuilder) AddToolUse(toolName, filePath, content string) string {
	b.toolUseCounter++
	toolUseID := fmt.Sprintf("toolu_%d", b.toolUseCounter)

	toolUse := map[string]interface{}{
		"type": "tool_use",
		"id":   toolUseID,
		"name": toolName,
		"input": map[string]interface{}{
			"file_path": filePath,
			"content":   content,
		},
	}

	b.messages = append(b.messages, map[string]interface{}{
		"uuid": fmt.Sprintf("asst-%d", len(b.messages)+1),
		"type": "assistant",
		"message": map[string]interface{}{
			"content": []interface{}{toolUse},
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	return toolUseID
}

// AddToolResult adds a tool result for a previous tool use.
func (b *TranscriptBuilder) AddToolResult(toolUseID string) *TranscriptBuilder {
	b.messages = append(b.messages, map[string]interface{}{
		"uuid": fmt.Sprintf("user-%d", len(b.messages)+1),
		"type": "user",
		"message": map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     "Success",
				},
			},
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	return b
}

// AddTaskToolUse adds a Task tool invocation (for subagent calls).
// Returns the tool use ID for use with AddTaskToolResult.
func (b *TranscriptBuilder) AddTaskToolUse(toolUseID, prompt string) string {
	if toolUseID == "" {
		b.toolUseCounter++
		toolUseID = fmt.Sprintf("toolu_%d", b.toolUseCounter)
	}

	toolUse := map[string]interface{}{
		"type": "tool_use",
		"id":   toolUseID,
		"name": "Task",
		"input": map[string]interface{}{
			"prompt":        prompt,
			"subagent_type": "general-purpose",
		},
	}

	b.messages = append(b.messages, map[string]interface{}{
		"uuid": fmt.Sprintf("asst-%d", len(b.messages)+1),
		"type": "assistant",
		"message": map[string]interface{}{
			"content": []interface{}{toolUse},
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	return toolUseID
}

// AddTaskToolResult adds a tool result for a Task tool invocation.
// The UUID of this message is used as the checkpoint UUID for rewind.
func (b *TranscriptBuilder) AddTaskToolResult(toolUseID, agentID string) string {
	uuid := fmt.Sprintf("user-%d", len(b.messages)+1)

	b.messages = append(b.messages, map[string]interface{}{
		"uuid": uuid,
		"type": "user",
		"message": map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     "agentId: " + agentID,
				},
			},
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	return uuid
}

// WriteToFile writes the transcript to a JSONL file.
func (b *TranscriptBuilder) WriteToFile(path string) error {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = file.Close() }()

	encoder := json.NewEncoder(file)
	for _, msg := range b.messages {
		if err := encoder.Encode(msg); err != nil {
			return fmt.Errorf("failed to encode message: %w", err)
		}
	}
	return nil
}

// String returns the transcript as a JSONL string.
func (b *TranscriptBuilder) String() string {
	var result string
	var resultSb176 strings.Builder
	for _, msg := range b.messages {
		data, _ := json.Marshal(msg)
		resultSb176.WriteString(string(data) + "\n")
	}
	result += resultSb176.String()
	return result
}

// LastUUID returns the UUID of the last message added.
func (b *TranscriptBuilder) LastUUID() string {
	if len(b.messages) == 0 {
		return ""
	}
	return b.messages[len(b.messages)-1]["uuid"].(string)
}
