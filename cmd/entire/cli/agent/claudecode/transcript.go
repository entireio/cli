package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/agent"
)

// Transcript parsing types - Claude Code uses JSONL format

// TranscriptLine represents a single line in Claude's JSONL transcript
type TranscriptLine struct {
	Type    string          `json:"type"`
	UUID    string          `json:"uuid"`
	Message json.RawMessage `json:"message"`
}

// userMessage represents a user message in the transcript
type userMessage struct {
	Content interface{} `json:"content"`
}

// assistantMessage represents an assistant message in the transcript
type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

// contentBlock represents a block within an assistant message
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// toolInput represents the input to file modification tools
type toolInput struct {
	FilePath     string `json:"file_path,omitempty"`
	NotebookPath string `json:"notebook_path,omitempty"`
}

// Scanner buffer size for large transcript files (10MB)
const scannerBufferSize = 10 * 1024 * 1024

// ParseTranscript parses raw JSONL content into transcript lines
func ParseTranscript(data []byte) ([]TranscriptLine, error) {
	var lines []TranscriptLine
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, scannerBufferSize), scannerBufferSize)

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

// SerializeTranscript converts transcript lines back to JSONL bytes
func SerializeTranscript(lines []TranscriptLine) ([]byte, error) {
	var buf bytes.Buffer
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal line: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ExtractModifiedFiles extracts files modified by tool calls from transcript
func ExtractModifiedFiles(lines []TranscriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range lines {
		if line.Type != "assistant" {
			continue
		}

		var msg assistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type != "tool_use" {
				continue
			}

			// Check if it's a file modification tool
			isModifyTool := false
			for _, name := range FileModificationTools {
				if block.Name == name {
					isModifyTool = true
					break
				}
			}

			if !isModifyTool {
				continue
			}

			var input toolInput
			if err := json.Unmarshal(block.Input, &input); err != nil {
				continue
			}

			file := input.FilePath
			if file == "" {
				file = input.NotebookPath
			}

			if file != "" && !fileSet[file] {
				fileSet[file] = true
				files = append(files, file)
			}
		}
	}

	return files
}

// ExtractLastUserPrompt extracts the last user message from transcript
func ExtractLastUserPrompt(lines []TranscriptLine) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Type != "user" {
			continue
		}

		var msg userMessage
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

// TruncateAtUUID returns transcript lines up to and including the line with given UUID
func TruncateAtUUID(lines []TranscriptLine, uuid string) []TranscriptLine {
	if uuid == "" {
		return lines
	}

	for i, line := range lines {
		if line.UUID == uuid {
			return lines[:i+1]
		}
	}

	// UUID not found, return full transcript
	return lines
}

// toolResultBlock represents a tool_result in a user message
type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
}

// userMessageWithToolResults represents a user message that may contain tool results
type userMessageWithToolResults struct {
	Content []toolResultBlock `json:"content"`
}

// FindCheckpointUUID finds the UUID of the message containing the tool_result
// for the given tool_use_id
func FindCheckpointUUID(lines []TranscriptLine, toolUseID string) (string, bool) {
	for _, line := range lines {
		if line.Type != "user" {
			continue
		}

		var msg userMessageWithToolResults
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID {
				return line.UUID, true
			}
		}
	}
	return "", false
}

// CalculateTokenUsage calculates token usage from a Claude Code transcript.
// This is specific to Claude/Anthropic's API format where each assistant message
// contains a usage object with input_tokens, output_tokens, and cache tokens.
//
// Due to streaming, multiple transcript rows may share the same message.id.
// We deduplicate by taking the row with the highest output_tokens for each message.id.
func CalculateTokenUsage(transcript []TranscriptLine) *agent.TokenUsage {
	// Map from message.id to the usage with highest output_tokens
	usageByMessageID := make(map[string]messageUsage)

	for _, line := range transcript {
		if line.Type != "assistant" {
			continue
		}

		var msg messageWithUsage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		if msg.ID == "" {
			continue
		}

		// Keep the entry with highest output_tokens (final streaming state)
		existing, exists := usageByMessageID[msg.ID]
		if !exists || msg.Usage.OutputTokens > existing.OutputTokens {
			usageByMessageID[msg.ID] = msg.Usage
		}
	}

	// Sum up all unique messages
	usage := &agent.TokenUsage{
		APICallCount: len(usageByMessageID),
	}
	for _, u := range usageByMessageID {
		usage.InputTokens += u.InputTokens
		usage.CacheCreationTokens += u.CacheCreationInputTokens
		usage.CacheReadTokens += u.CacheReadInputTokens
		usage.OutputTokens += u.OutputTokens
	}

	return usage
}

// CalculateTokenUsageFromFile calculates token usage from a Claude Code transcript file.
// If startLine > 0, only considers lines from startLine onwards.
func CalculateTokenUsageFromFile(path string, startLine int) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}

	transcript, err := parseTranscriptFromLine(path, startLine)
	if err != nil {
		return nil, err
	}

	return CalculateTokenUsage(transcript), nil
}

// parseTranscriptFromLine parses a transcript file starting from a specific line.
func parseTranscriptFromLine(path string, startLine int) ([]TranscriptLine, error) {
	file, err := os.Open(path) //nolint:gosec // Path comes from Claude Code transcript location
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	var lines []TranscriptLine
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, scannerBufferSize), scannerBufferSize)

	lineNum := 0
	for scanner.Scan() {
		if lineNum < startLine {
			lineNum++
			continue
		}
		lineNum++

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

// ExtractSpawnedAgentIDs extracts agent IDs from Task tool results in a transcript.
// When a Task tool completes, the tool_result contains "agentId: <id>" in its content.
// Returns a map of agentID -> toolUseID for all spawned agents.
func ExtractSpawnedAgentIDs(transcript []TranscriptLine) map[string]string {
	agentIDs := make(map[string]string)

	for _, line := range transcript {
		if line.Type != "user" {
			continue
		}

		// Parse as array of content blocks (tool results)
		var contentBlocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}

		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		if err := json.Unmarshal(msg.Content, &contentBlocks); err != nil {
			continue
		}

		for _, block := range contentBlocks {
			if block.Type != "tool_result" {
				continue
			}

			// Content can be a string or array of text blocks
			var textContent string

			// Try as array of text blocks first
			var textBlocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(block.Content, &textBlocks); err == nil {
				var textContentSb361 strings.Builder
				for _, tb := range textBlocks {
					if tb.Type == "text" {
						textContentSb361.WriteString(tb.Text + "\n")
					}
				}
				textContent += textContentSb361.String()
			} else {
				// Try as plain string
				var str string
				if err := json.Unmarshal(block.Content, &str); err == nil {
					textContent = str
				}
			}

			// Look for agentId in the text
			if agentID := extractAgentIDFromText(textContent); agentID != "" {
				agentIDs[agentID] = block.ToolUseID
			}
		}
	}

	return agentIDs
}

// extractAgentIDFromText extracts an agent ID from text containing "agentId: <id>".
func extractAgentIDFromText(text string) string {
	const prefix = "agentId: "
	idx := strings.Index(text, prefix)
	if idx == -1 {
		return ""
	}

	// Extract the ID (alphanumeric characters after the prefix)
	start := idx + len(prefix)
	end := start
	for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' ||
		text[end] >= 'A' && text[end] <= 'Z' ||
		text[end] >= '0' && text[end] <= '9') {
		end++
	}

	if end > start {
		return text[start:end]
	}
	return ""
}

// CalculateTotalTokenUsage calculates token usage for a turn, including subagents.
// It parses the main transcript from startLine, extracts spawned agent IDs,
// and calculates their token usage from transcripts in subagentsDir.
func CalculateTotalTokenUsage(transcriptPath string, startLine int, subagentsDir string) (*agent.TokenUsage, error) {
	// Calculate main transcript usage
	mainUsage, err := CalculateTokenUsageFromFile(transcriptPath, startLine)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate main token usage: %w", err)
	}

	// Parse the transcript to find spawned agents
	transcript, err := parseTranscriptFromLine(transcriptPath, startLine)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for agent IDs: %w", err)
	}

	agentIDs := ExtractSpawnedAgentIDs(transcript)

	// Calculate subagent token usage
	if len(agentIDs) > 0 {
		subagentUsage := &agent.TokenUsage{}
		for agentID := range agentIDs {
			agentPath := filepath.Join(subagentsDir, fmt.Sprintf("agent-%s.jsonl", agentID))
			agentUsage, err := CalculateTokenUsageFromFile(agentPath, 0)
			if err != nil {
				// Agent transcript may not exist yet or may have been cleaned up
				continue
			}
			subagentUsage.InputTokens += agentUsage.InputTokens
			subagentUsage.CacheCreationTokens += agentUsage.CacheCreationTokens
			subagentUsage.CacheReadTokens += agentUsage.CacheReadTokens
			subagentUsage.OutputTokens += agentUsage.OutputTokens
			subagentUsage.APICallCount += agentUsage.APICallCount
		}
		if subagentUsage.APICallCount > 0 {
			mainUsage.SubagentTokens = subagentUsage
		}
	}

	return mainUsage, nil
}
