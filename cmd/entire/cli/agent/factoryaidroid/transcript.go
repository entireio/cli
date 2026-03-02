package factoryaidroid

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// TranscriptLine is an alias to the shared transcript.Line type.
type TranscriptLine = transcript.Line

// droidEnvelope is the top-level structure of a Factory AI Droid JSONL line.
// Droid wraps messages as {"type":"message","id":"...","message":{"role":"assistant","content":[...]}},
// unlike Claude Code which uses {"type":"assistant","uuid":"...","message":{"content":[...]}}.
type droidEnvelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Message json.RawMessage `json:"message"`
}

// droidMessageRole extracts just the role from the inner message.
type droidMessageRole struct {
	Role string `json:"role"`
}

// ParseDroidTranscript parses a Droid JSONL file into normalized transcript.Line entries.
// It transforms the Droid envelope format (type="message", role inside message) into the
// shared transcript.Line format (type="assistant"/"user", message=inner content).
// Non-message entries (session_start, etc.) are skipped.
func ParseDroidTranscript(path string, startLine int) ([]transcript.Line, int, error) {
	file, err := os.Open(path) //nolint:gosec // path is a controlled transcript file path
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	return parseDroidTranscriptFromReader(file, startLine)
}

// ParseDroidTranscriptFromBytes parses Droid JSONL content from a byte slice.
// startLine skips the first N raw JSONL lines before parsing (0 = parse all).
// Returns parsed lines, total raw line count, and any error.
// This mirrors ParseDroidTranscript's startLine parameter, applying the offset
// at the raw line level before filtering out non-message entries.
func ParseDroidTranscriptFromBytes(content []byte, startLine int) ([]transcript.Line, int, error) {
	return parseDroidTranscriptFromReader(bytes.NewReader(content), startLine)
}

func parseDroidTranscriptFromReader(r io.Reader, startLine int) ([]transcript.Line, int, error) {
	reader := bufio.NewReader(r)
	var lines []transcript.Line
	totalLines := 0

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", err)
		}

		if len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			continue
		}

		if totalLines >= startLine {
			if line, ok := parseDroidLine(lineBytes); ok {
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

// parseDroidLine converts a single Droid JSONL line into a normalized transcript.Line.
// Returns false if the line is not a message entry (e.g., session_start).
func parseDroidLine(lineBytes []byte) (transcript.Line, bool) {
	var env droidEnvelope
	if err := json.Unmarshal(lineBytes, &env); err != nil {
		return transcript.Line{}, false
	}

	// Only process "message" type entries — skip session_start, etc.
	if env.Type != "message" {
		return transcript.Line{}, false
	}

	// Extract role from the inner message
	var role droidMessageRole
	if err := json.Unmarshal(env.Message, &role); err != nil {
		return transcript.Line{}, false
	}

	return transcript.Line{
		Type:    role.Role, // "assistant" or "user"
		UUID:    env.ID,
		Message: env.Message,
	}, true
}

// ExtractModifiedFiles extracts files modified by tool calls from transcript.
func ExtractModifiedFiles(lines []TranscriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range lines {
		if line.Type != "assistant" {
			continue
		}

		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type != "tool_use" || !slices.Contains(FileModificationTools, block.Name) {
				continue
			}

			var input transcript.ToolInput
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

// CalculateTokenUsage calculates token usage from a Factory AI Droid transcript.
// Due to streaming, multiple transcript rows may share the same message.id.
// We deduplicate by taking the row with the highest output_tokens for each message.id.
func CalculateTokenUsage(transcriptLines []TranscriptLine) *agent.TokenUsage {
	// Map from message.id to the usage with highest output_tokens
	usageByMessageID := make(map[string]messageUsage)

	for _, line := range transcriptLines {
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

// CalculateTokenUsageFromFile calculates token usage from a transcript file.
// If startLine > 0, only considers lines from startLine onwards.
func CalculateTokenUsageFromFile(path string, startLine int) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}

	lines, _, err := ParseDroidTranscript(path, startLine)
	if err != nil {
		return nil, err
	}

	return CalculateTokenUsage(lines), nil
}

// ExtractSpawnedAgentIDs extracts agent IDs from Task tool results in a transcript.
// When a Task tool completes, the tool_result contains "agentId: <id>" in its content.
// Returns a map of agentID -> toolUseID for all spawned agents.
func ExtractSpawnedAgentIDs(transcriptLines []TranscriptLine) map[string]string {
	agentIDs := make(map[string]string)

	for _, line := range transcriptLines {
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
				var sb strings.Builder
				for _, tb := range textBlocks {
					if tb.Type == "text" {
						sb.WriteString(tb.Text + "\n")
					}
				}
				textContent = sb.String()
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

// CalculateTotalTokenUsageFromBytes calculates token usage from pre-loaded transcript bytes,
// including subagents. It parses the main transcript bytes from startLine, extracts spawned
// agent IDs, and calculates their token usage from transcript files in subagentsDir.
func CalculateTotalTokenUsageFromBytes(data []byte, startLine int, subagentsDir string) (*agent.TokenUsage, error) {
	if len(data) == 0 {
		return &agent.TokenUsage{}, nil
	}

	parsed, _, err := ParseDroidTranscriptFromBytes(data, startLine)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	mainUsage := CalculateTokenUsage(parsed)

	agentIDs := ExtractSpawnedAgentIDs(parsed)
	if len(agentIDs) > 0 && subagentsDir != "" {
		subagentUsage := &agent.TokenUsage{}
		for agentID := range agentIDs {
			agentPath := filepath.Join(subagentsDir, fmt.Sprintf("agent-%s.jsonl", agentID))
			agentUsage, err := CalculateTokenUsageFromFile(agentPath, 0)
			if err != nil {
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

// ExtractAllModifiedFilesFromBytes extracts files modified by both the main agent and
// any subagents from pre-loaded transcript bytes. It parses the main transcript bytes from
// startLine, collects modified files, then reads each subagent's transcript from
// subagentsDir to collect their modified files too.
func ExtractAllModifiedFilesFromBytes(data []byte, startLine int, subagentsDir string) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}

	parsed, _, err := ParseDroidTranscriptFromBytes(data, startLine)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	files := ExtractModifiedFiles(parsed)
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	agentIDs := ExtractSpawnedAgentIDs(parsed)
	if subagentsDir == "" {
		return files, nil
	}
	for agentID := range agentIDs {
		agentPath := filepath.Join(subagentsDir, fmt.Sprintf("agent-%s.jsonl", agentID))
		agentLines, _, agentErr := ParseDroidTranscript(agentPath, 0)
		if agentErr != nil {
			continue
		}
		for _, f := range ExtractModifiedFiles(agentLines) {
			if !fileSet[f] {
				fileSet[f] = true
				files = append(files, f)
			}
		}
	}

	return files, nil
}

// CalculateTotalTokenUsageFromTranscript calculates token usage for a turn, including subagents.
// It parses the main transcript from startLine, extracts spawned agent IDs,
// and calculates their token usage from transcripts in subagentsDir.
func CalculateTotalTokenUsageFromTranscript(transcriptPath string, startLine int, subagentsDir string) (*agent.TokenUsage, error) {
	if transcriptPath == "" {
		return &agent.TokenUsage{}, nil
	}

	// Parse transcript once using Droid-specific parser
	parsed, _, err := ParseDroidTranscript(transcriptPath, startLine)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Calculate token usage from parsed transcript
	mainUsage := CalculateTokenUsage(parsed)

	// Extract spawned agent IDs from the same parsed transcript
	agentIDs := ExtractSpawnedAgentIDs(parsed)

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

// ExtractAllModifiedFilesFromTranscript extracts files modified by both the main agent and
// any subagents spawned via the Task tool. It parses the main transcript from
// startLine, collects modified files from the main agent, then reads each
// subagent's transcript from subagentsDir to collect their modified files too.
// The result is a deduplicated list of all modified file paths.
func ExtractAllModifiedFilesFromTranscript(transcriptPath string, startLine int, subagentsDir string) ([]string, error) {
	if transcriptPath == "" {
		return nil, nil
	}

	// Parse main transcript once using Droid-specific parser
	parsed, _, err := ParseDroidTranscript(transcriptPath, startLine)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Collect modified files from main agent (already deduplicated)
	files := ExtractModifiedFiles(parsed)
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	// Find spawned subagents and collect their modified files
	agentIDs := ExtractSpawnedAgentIDs(parsed)
	for agentID := range agentIDs {
		agentPath := filepath.Join(subagentsDir, fmt.Sprintf("agent-%s.jsonl", agentID))
		agentLines, _, agentErr := ParseDroidTranscript(agentPath, 0)
		if agentErr != nil {
			// Subagent transcript may not exist yet or may have been cleaned up
			continue
		}
		for _, f := range ExtractModifiedFiles(agentLines) {
			if !fileSet[f] {
				fileSet[f] = true
				files = append(files, f)
			}
		}
	}

	return files, nil
}
