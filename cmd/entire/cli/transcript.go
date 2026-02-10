package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Transcript message type constants - aliases to transcript package for local use.
const (
	transcriptTypeUser      = transcript.TypeUser
	transcriptTypeAssistant = transcript.TypeAssistant
	contentTypeText         = transcript.ContentTypeText
	contentTypeToolUse      = transcript.ContentTypeToolUse
)

// parseTranscript reads and parses a Claude Code transcript file.
// Uses bufio.Reader to handle arbitrarily long lines.
func parseTranscript(path string) ([]transcriptLine, error) {
	file, err := os.Open(path) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return nil, err //nolint:wrapcheck // already present in codebase
	}
	defer func() { _ = file.Close() }()

	var lines []transcriptLine
	reader := bufio.NewReader(file)

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read transcript: %w", err)
		}

		// Handle empty line or EOF without content
		if len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			continue
		}

		var line transcriptLine
		if err := json.Unmarshal(lineBytes, &line); err == nil {
			lines = append(lines, line)
		}

		if err == io.EOF {
			break
		}
	}

	return lines, nil
}

// parseTranscriptFromLine reads and parses a transcript file starting from a specific line.
// Uses bufio.Reader to handle arbitrarily long lines.
// Returns:
//   - lines: parsed transcript lines from startLine onwards (malformed lines skipped)
//   - totalLines: total number of lines in the file (including malformed ones)
//   - error: any error encountered during reading
//
// The startLine parameter is 0-indexed (startLine=0 reads from the beginning).
// This is useful for incremental parsing when you've already processed some lines.
func parseTranscriptFromLine(path string, startLine int) ([]transcriptLine, int, error) {
	file, err := os.Open(path) //nolint:gosec // path is a controlled transcript file path
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	var lines []transcriptLine
	reader := bufio.NewReader(file)

	totalLines := 0
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", err)
		}

		// Handle empty line or EOF without content
		if len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			continue
		}

		// Count all lines for totalLines, but only parse after startLine
		if totalLines >= startLine {
			var line transcriptLine
			if err := json.Unmarshal(lineBytes, &line); err == nil {
				lines = append(lines, line)
			}
		}
		totalLines++

		if err == io.EOF {
			break
		}
	}

	return lines, totalLines, nil
}

// parseTranscriptFromBytes parses transcript content from a byte slice.
// Uses bufio.Reader to handle arbitrarily long lines.
func parseTranscriptFromBytes(content []byte) ([]transcriptLine, error) {
	lines, err := transcript.ParseFromBytes(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	return lines, nil
}

// PromptResponsePair represents a user prompt and the assistant's responses.
// Used for extracting all interactions from a transcript.
type PromptResponsePair struct {
	Prompt    string
	Responses []string // Multiple responses can occur between tool calls
	Files     []string
}

// ExtractAllPromptResponses extracts all user prompt and assistant response pairs from a transcript.
// Returns pairs in chronological order (oldest first).
// Each pair contains the user's prompt, assistant's text response, and files modified.
func ExtractAllPromptResponses(transcript []transcriptLine) []PromptResponsePair {
	var pairs []PromptResponsePair

	// Find all user prompts (messages with string content, not tool results)
	var userIndices []int
	for i, line := range transcript {
		if line.Type == transcriptTypeUser {
			var msg userMessage
			if err := json.Unmarshal(line.Message, &msg); err != nil {
				continue
			}
			// Only count messages with string content (real user prompts)
			if _, ok := msg.Content.(string); ok {
				userIndices = append(userIndices, i)
			} else if arr, ok := msg.Content.([]interface{}); ok {
				// Check if array has text blocks (also a real user prompt)
				for _, item := range arr {
					if m, ok := item.(map[string]interface{}); ok {
						if m["type"] == contentTypeText {
							userIndices = append(userIndices, i)
							break
						}
					}
				}
			}
		}
	}

	// For each user prompt, extract the prompt text, following assistant response, and files
	for idx, userIdx := range userIndices {
		// Determine the range for this prompt's conversation
		endIdx := len(transcript)
		if idx < len(userIndices)-1 {
			endIdx = userIndices[idx+1]
		}

		// Extract prompt text
		prompt := extractUserPromptAt(transcript, userIdx)
		if prompt == "" {
			continue
		}

		// Extract assistant responses and files from this range
		slice := transcript[userIdx:endIdx]
		responses := extractAssistantResponses(slice)
		files := extractModifiedFiles(slice)

		pairs = append(pairs, PromptResponsePair{
			Prompt:    prompt,
			Responses: responses,
			Files:     files,
		})
	}

	return pairs
}

// extractUserPromptAt extracts the user prompt at the given index.
// IDE-injected context tags (like <ide_opened_file>) are stripped from the result.
func extractUserPromptAt(lines []transcriptLine, idx int) string {
	if idx >= len(lines) || lines[idx].Type != transcriptTypeUser {
		return ""
	}

	return transcript.ExtractUserContent(lines[idx].Message)
}

// extractAssistantResponses collects all assistant text blocks from the given transcript slice.
// A single prompt can trigger multiple assistant responses interspersed with tool calls.
func extractAssistantResponses(transcript []transcriptLine) []string {
	var texts []string
	for _, line := range transcript {
		if line.Type == transcriptTypeAssistant {
			var msg assistantMessage
			if err := json.Unmarshal(line.Message, &msg); err != nil {
				continue
			}

			for _, block := range msg.Content {
				if block.Type == contentTypeText && block.Text != "" {
					texts = append(texts, block.Text)
				}
			}
		}
	}
	return texts
}

// extractUserPrompts extracts all user messages from the transcript in order.
// Only considers messages with string content (not tool results).
// IDE-injected context tags (like <ide_opened_file>) are stripped from the results.
func extractUserPrompts(transcript []transcriptLine) []string {
	var prompts []string
	for i := range transcript {
		if transcript[i].Type != transcriptTypeUser {
			continue
		}
		var msg userMessage
		if err := json.Unmarshal(transcript[i].Message, &msg); err != nil {
			continue
		}

		// Handle string content
		if str, ok := msg.Content.(string); ok {
			prompts = append(prompts, textutil.StripIDEContextTags(str))
			continue
		}

		// Handle array content (only if it contains text blocks)
		if arr, ok := msg.Content.([]interface{}); ok {
			var texts []string
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if m["type"] == contentTypeText {
						if text, ok := m["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
			}
			if len(texts) > 0 {
				prompts = append(prompts, textutil.StripIDEContextTags(strings.Join(texts, "\n\n")))
			}
		}
	}
	return prompts
}

// extractLastUserPrompt extracts the last user message from the transcript.
// Only considers messages with string content (not tool results).
// IDE-injected context tags (like <ide_opened_file>) are stripped from the result.
func extractLastUserPrompt(transcript []transcriptLine) string {
	prompts := extractUserPrompts(transcript)
	if len(prompts) == 0 {
		return ""
	}
	return prompts[len(prompts)-1]
}

// extractLastAssistantMessage extracts the last text message from the assistant
func extractLastAssistantMessage(transcript []transcriptLine) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Type == transcriptTypeAssistant {
			var msg assistantMessage
			if err := json.Unmarshal(transcript[i].Message, &msg); err != nil {
				continue
			}

			for _, block := range msg.Content {
				if block.Type == contentTypeText && block.Text != "" {
					return block.Text
				}
			}
		}
	}
	return ""
}

// findLastUserUUID finds the UUID of the last user message (that has string content)
func findLastUserUUID(transcript []transcriptLine) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Type == transcriptTypeUser {
			var msg userMessage
			if err := json.Unmarshal(transcript[i].Message, &msg); err != nil {
				continue
			}
			// Only count messages with string content (not tool results)
			if _, ok := msg.Content.(string); ok {
				return transcript[i].UUID
			}
		}
	}
	return ""
}

// filterTranscriptAfterUUID returns transcript lines after the given UUID
func filterTranscriptAfterUUID(transcript []transcriptLine, uuid string) []transcriptLine {
	if uuid == "" {
		return transcript
	}

	foundIndex := -1
	for i, line := range transcript {
		if line.UUID == uuid {
			foundIndex = i
			break
		}
	}

	if foundIndex == -1 || foundIndex == len(transcript)-1 {
		return transcript
	}

	return transcript[foundIndex+1:]
}

// extractModifiedFiles extracts the list of files modified by tool calls
func extractModifiedFiles(transcript []transcriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range transcript {
		if line.Type == transcriptTypeAssistant {
			var msg assistantMessage
			if err := json.Unmarshal(line.Message, &msg); err != nil {
				continue
			}

			for _, block := range msg.Content {
				if block.Type == contentTypeToolUse {
					isModifyTool := false
					for _, name := range claudecode.FileModificationTools {
						if block.Name == name {
							isModifyTool = true
							break
						}
					}

					if isModifyTool {
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
			}
		}
	}

	return files
}

// extractKeyActions extracts key actions from the transcript for context file
func extractKeyActions(transcript []transcriptLine, maxActions int) []string {
	var keyActions []string

	for _, line := range transcript {
		if line.Type == transcriptTypeAssistant {
			var msg assistantMessage
			if err := json.Unmarshal(line.Message, &msg); err != nil {
				continue
			}

			for _, block := range msg.Content {
				if block.Type == contentTypeToolUse {
					var input toolInput
					_ = json.Unmarshal(block.Input, &input) //nolint:errcheck // Best-effort parsing for display purposes

					detail := input.Description
					if detail == "" {
						detail = input.Command
					}
					if detail == "" {
						detail = input.FilePath
					}
					if detail == "" {
						detail = input.Pattern
					}

					action := "- **" + block.Name + "**: " + detail
					keyActions = append(keyActions, action)
					if len(keyActions) >= maxActions {
						return keyActions
					}
				}
			}
		}
	}

	return keyActions
}

// AgentTranscriptPath returns the path to a subagent's transcript file.
// Subagent transcripts are stored as agent-{agentId}.jsonl in the same directory
// as the main transcript.
func AgentTranscriptPath(transcriptDir, agentID string) string {
	return filepath.Join(transcriptDir, fmt.Sprintf("agent-%s.jsonl", agentID))
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
// for the given tool_use_id. This is used to find the checkpoint point for
// transcript truncation when rewinding to a task.
// Returns the UUID and true if found, empty string and false otherwise.
func FindCheckpointUUID(transcript []transcriptLine, toolUseID string) (string, bool) {
	for _, line := range transcript {
		if line.Type != transcriptTypeUser {
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

// TruncateTranscriptAtUUID returns transcript lines up to and including the
// line with the given UUID. If the UUID is not found or is empty, returns
// the entire transcript.
//
//nolint:revive // Exported for testing purposes
func TruncateTranscriptAtUUID(transcript []transcriptLine, uuid string) []transcriptLine {
	if uuid == "" {
		return transcript
	}

	for i, line := range transcript {
		if line.UUID == uuid {
			return transcript[:i+1]
		}
	}

	// UUID not found, return full transcript
	return transcript
}

// writeTranscript writes transcript lines to a file in JSONL format.
func writeTranscript(path string, transcript []transcriptLine) error {
	file, err := os.Create(path) //nolint:gosec // Writing to controlled git metadata path
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = file.Close() }()

	for _, line := range transcript {
		data, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("failed to marshal line: %w", err)
		}
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("failed to write line: %w", err)
		}
		if _, err := file.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write newline: %w", err)
		}
	}

	return nil
}

// TranscriptPosition contains the position information for a transcript file.
type TranscriptPosition struct {
	LastUUID  string // Last non-empty UUID (from user/assistant messages)
	LineCount int    // Total number of lines
}

// GetTranscriptPosition reads a transcript file and returns the last UUID and line count.
// Returns empty position if file doesn't exist or is empty.
// Only considers UUIDs from actual messages (user/assistant), not summary rows which use leafUuid.
func GetTranscriptPosition(path string) (TranscriptPosition, error) {
	if path == "" {
		return TranscriptPosition{}, nil
	}

	file, err := os.Open(path) //nolint:gosec // Reading from controlled transcript path
	if err != nil {
		if os.IsNotExist(err) {
			return TranscriptPosition{}, nil
		}
		return TranscriptPosition{}, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	var pos TranscriptPosition
	reader := bufio.NewReader(file)

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return TranscriptPosition{}, fmt.Errorf("failed to read transcript: %w", err)
		}

		if len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			continue
		}

		pos.LineCount++

		// Parse line to extract UUID (only from user/assistant messages, not summaries)
		var line transcriptLine
		if err := json.Unmarshal(lineBytes, &line); err == nil {
			if line.UUID != "" {
				pos.LastUUID = line.UUID
			}
		}

		if err == io.EOF {
			break
		}
	}

	return pos, nil
}
