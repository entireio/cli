package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// TranscriptLine is an alias to the shared transcript.Line type.
type TranscriptLine = transcript.Line

// OpenCode storage directory structure:
// ~/.local/share/opencode/storage/
// ├── message/
// │   └── ses_<session-id>/
// │       └── msg_<id>.json       # Message metadata (role, tokens, timestamps)
// ├── part/
// │   └── msg_<id>/               # Parts directory per message
// │       └── prt_<id>.json       # Part content (text, tool calls, etc.)
// └── session/
//     └── <project-hash>/         # Session metadata per project

// MessageMetadata represents the metadata stored in message/*.json files.
type MessageMetadata struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ParentID  string `json:"parentID,omitempty"`
	Time      struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time"`
	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Finish     string `json:"finish,omitempty"`
	ModelID    string `json:"modelID,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
}

// MessagePart represents a part stored in part/msg_<id>/*.json files.
type MessagePart struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	// For text type
	Text string `json:"text,omitempty"`
	// For tool type
	CallID string `json:"callID,omitempty"`
	Tool   string `json:"tool,omitempty"`
	State  *struct {
		Status string                 `json:"status,omitempty"`
		Input  map[string]interface{} `json:"input,omitempty"`
		Output interface{}            `json:"output,omitempty"`
		Title  string                 `json:"title,omitempty"`
		Time   struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time,omitempty"`
	} `json:"state,omitempty"`
	// For step parts
	Reason string `json:"reason,omitempty"`
	Tokens *struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens,omitempty"`
	Cost float64 `json:"cost,omitempty"`
}

// GetStorageDir returns the OpenCode storage directory path.
// On macOS/Linux: ~/.local/share/opencode/storage
// Respects XDG_DATA_HOME if set.
func GetStorageDir() (string, error) {
	// Check for XDG_DATA_HOME first
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, "opencode", "storage"), nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Platform-specific default
	switch runtime.GOOS {
	case "darwin", "linux":
		return filepath.Join(homeDir, ".local", "share", "opencode", "storage"), nil
	case "windows":
		// On Windows, use AppData\Local
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, "opencode", "storage"), nil
		}
		return filepath.Join(homeDir, "AppData", "Local", "opencode", "storage"), nil
	default:
		return filepath.Join(homeDir, ".local", "share", "opencode", "storage"), nil
	}
}

// ReconstructTranscript reads OpenCode storage and reconstructs a JSONL transcript
// compatible with Entire's transcript format.
func ReconstructTranscript(sessionID string) ([]byte, error) {
	storageDir, err := GetStorageDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get storage directory: %w", err)
	}

	// Read all messages for this session
	messagesDir := filepath.Join(storageDir, "message", sessionID)
	if _, err := os.Stat(messagesDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("session not found in OpenCode storage: %s", sessionID)
	}

	entries, err := os.ReadDir(messagesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read messages directory: %w", err)
	}

	// Load all messages
	var messages []MessageMetadata
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		msgPath := filepath.Join(messagesDir, entry.Name())
		data, err := os.ReadFile(msgPath) //nolint:gosec // Path is constructed from session ID
		if err != nil {
			continue
		}

		var msg MessageMetadata
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		messages = append(messages, msg)
	}

	// Sort messages by creation time
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Time.Created < messages[j].Time.Created
	})

	// Convert to transcript lines
	var lines []TranscriptLine
	partsDir := filepath.Join(storageDir, "part")

	for _, msg := range messages {
		line, err := messageToTranscriptLine(msg, partsDir)
		if err != nil {
			continue // Skip malformed messages
		}
		lines = append(lines, line)
	}

	return SerializeTranscript(lines)
}

// messageToTranscriptLine converts an OpenCode message to a transcript line.
func messageToTranscriptLine(msg MessageMetadata, partsDir string) (TranscriptLine, error) {
	parts, err := loadMessageParts(msg.ID, partsDir)
	if err != nil {
		return TranscriptLine{}, err
	}

	var messageContent interface{}

	switch msg.Role {
	case "user":
		// Extract text content from parts
		var textContent string
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				textContent = part.Text
				break
			}
		}
		messageContent = transcript.UserMessage{
			Content: textContent,
		}

	case "assistant":
		// Build content blocks from parts
		var contentBlocks []transcript.ContentBlock
		for _, part := range parts {
			switch part.Type {
			case "text":
				if part.Text != "" {
					contentBlocks = append(contentBlocks, transcript.ContentBlock{
						Type: "text",
						Text: part.Text,
					})
				}
			case "tool":
				if part.Tool != "" && part.State != nil {
					// Convert tool input to JSON
					inputJSON, _ := json.Marshal(part.State.Input)
					contentBlocks = append(contentBlocks, transcript.ContentBlock{
						Type:  "tool_use",
						Name:  part.Tool,
						Input: inputJSON,
					})
				}
			}
		}

		// Add token usage info
		messageContent = map[string]interface{}{
			"content": contentBlocks,
			"id":      msg.ID,
			"model":   msg.ModelID,
			"usage": map[string]interface{}{
				"input_tokens":                msg.Tokens.Input,
				"output_tokens":               msg.Tokens.Output,
				"cache_creation_input_tokens": msg.Tokens.Cache.Write,
				"cache_read_input_tokens":     msg.Tokens.Cache.Read,
			},
		}
	}

	// Marshal the message content
	msgJSON, err := json.Marshal(messageContent)
	if err != nil {
		return TranscriptLine{}, fmt.Errorf("failed to marshal message: %w", err)
	}

	// Generate a UUID from the message ID
	uuid := msg.ID

	return TranscriptLine{
		Type:    msg.Role,
		UUID:    uuid,
		Message: msgJSON,
	}, nil
}

// loadMessageParts loads all parts for a message from the parts directory.
func loadMessageParts(messageID string, partsDir string) ([]MessagePart, error) {
	msgPartsDir := filepath.Join(partsDir, messageID)
	if _, err := os.Stat(msgPartsDir); os.IsNotExist(err) {
		return nil, nil // No parts is OK
	}

	entries, err := os.ReadDir(msgPartsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read parts directory: %w", err)
	}

	var parts []MessagePart
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		partPath := filepath.Join(msgPartsDir, entry.Name())
		data, err := os.ReadFile(partPath) //nolint:gosec // Path is constructed from message ID
		if err != nil {
			continue
		}

		var part MessagePart
		if err := json.Unmarshal(data, &part); err != nil {
			continue
		}
		parts = append(parts, part)
	}

	return parts, nil
}

// SerializeTranscript converts transcript lines to JSONL bytes.
func SerializeTranscript(lines []TranscriptLine) ([]byte, error) {
	var buf bytes.Buffer
	for _, line := range lines {
		// Add timestamp field for compatibility
		lineWithTimestamp := struct {
			Type      string          `json:"type"`
			UUID      string          `json:"uuid"`
			Message   json.RawMessage `json:"message"`
			Timestamp string          `json:"timestamp"`
		}{
			Type:      line.Type,
			UUID:      line.UUID,
			Message:   line.Message,
			Timestamp: time.Now().Format(time.RFC3339),
		}

		data, err := json.Marshal(lineWithTimestamp)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal line: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ParseTranscript parses raw JSONL content into transcript lines.
func ParseTranscript(data []byte) ([]TranscriptLine, error) {
	var lines []TranscriptLine
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Use a large buffer for potentially large lines
	const maxScannerBuffer = 10 * 1024 * 1024 // 10MB
	scanner.Buffer(make([]byte, 0, maxScannerBuffer), maxScannerBuffer)

	for scanner.Scan() {
		var line TranscriptLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue // Skip malformed lines
		}
		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan transcript: %w", err)
	}
	return lines, nil
}

// ExtractLastUserPrompt extracts the last user message from transcript lines.
func ExtractLastUserPrompt(lines []TranscriptLine) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Type != "user" {
			continue
		}

		var msg transcript.UserMessage
		if err := json.Unmarshal(lines[i].Message, &msg); err != nil {
			continue
		}

		// Handle string content
		if str, ok := msg.Content.(string); ok {
			return str
		}

		// Handle array content (text blocks)
		if arr, ok := msg.Content.([]interface{}); ok {
			var texts []string
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if m["type"] == "text" {
						if text, ok := m["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
			}
			if len(texts) > 0 {
				return strings.Join(texts, "\n\n")
			}
		}
	}
	return ""
}

// ExtractModifiedFiles extracts files modified by tool calls from transcript.
func ExtractModifiedFiles(lines []TranscriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range lines {
		if line.Type != "assistant" {
			continue
		}

		// Parse as map to access content
		var msg map[string]interface{}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}

		content, ok := contentRaw.([]interface{})
		if !ok {
			continue
		}

		for _, blockRaw := range content {
			block, ok := blockRaw.(map[string]interface{})
			if !ok {
				continue
			}

			blockType, _ := block["type"].(string)
			if blockType != "tool_use" {
				continue
			}

			toolName, _ := block["name"].(string)
			if !isFileModificationTool(toolName) {
				continue
			}

			inputRaw, ok := block["input"]
			if !ok {
				continue
			}

			// Input could be json.RawMessage or map
			var inputMap map[string]interface{}
			switch v := inputRaw.(type) {
			case json.RawMessage:
				if err := json.Unmarshal(v, &inputMap); err != nil {
					continue
				}
			case map[string]interface{}:
				inputMap = v
			default:
				continue
			}

			filePath, _ := inputMap["file_path"].(string)
			if filePath == "" {
				filePath, _ = inputMap["filePath"].(string)
			}
			if filePath == "" {
				filePath, _ = inputMap["path"].(string)
			}

			if filePath != "" && !fileSet[filePath] {
				fileSet[filePath] = true
				files = append(files, filePath)
			}
		}
	}

	return files
}

// FileModificationTools lists OpenCode tools that modify files.
var FileModificationTools = []string{
	"write",
	"edit",
	"bash", // Can modify files
}

// isFileModificationTool checks if a tool name is a file modification tool.
func isFileModificationTool(name string) bool {
	for _, t := range FileModificationTools {
		if name == t {
			return true
		}
	}
	return false
}

// CalculateTokenUsage calculates token usage from OpenCode transcript lines.
func CalculateTokenUsage(lines []TranscriptLine) *agent.TokenUsage {
	usage := &agent.TokenUsage{}

	// Track unique message IDs to avoid double-counting
	seenMessages := make(map[string]bool)

	for _, line := range lines {
		if line.Type != "assistant" {
			continue
		}

		// Parse as map to access usage
		var msg map[string]interface{}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		msgID, _ := msg["id"].(string)
		if msgID == "" || seenMessages[msgID] {
			continue
		}
		seenMessages[msgID] = true

		usageRaw, ok := msg["usage"].(map[string]interface{})
		if !ok {
			continue
		}

		if v, ok := usageRaw["input_tokens"].(float64); ok {
			usage.InputTokens += int(v)
		}
		if v, ok := usageRaw["output_tokens"].(float64); ok {
			usage.OutputTokens += int(v)
		}
		if v, ok := usageRaw["cache_creation_input_tokens"].(float64); ok {
			usage.CacheCreationTokens += int(v)
		}
		if v, ok := usageRaw["cache_read_input_tokens"].(float64); ok {
			usage.CacheReadTokens += int(v)
		}
		usage.APICallCount++
	}

	return usage
}
