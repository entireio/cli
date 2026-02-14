package codexcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Scanner buffer size for large transcript files (10MB)
const scannerBufferSize = 10 * 1024 * 1024

// ParseTranscript parses raw JSONL content into transcript lines.
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

// SerializeTranscript converts transcript lines back to JSONL bytes.
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

// ExtractModifiedFiles extracts files modified by exec_command tool calls.
// Unlike Claude/Gemini which use dedicated Write/Edit tools, Codex uses
// exec_command with shell commands. Detection is heuristic-based.
func ExtractModifiedFiles(lines []TranscriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range lines {
		if line.Type != eventTypeResponseItem {
			continue
		}

		var payload responseItemPayload
		if err := json.Unmarshal(line.Payload, &payload); err != nil {
			continue
		}

		if payload.Type != responseItemFunctionCall || payload.Name != "exec_command" {
			continue
		}

		var args execCommandArgs
		if err := json.Unmarshal([]byte(payload.Arguments), &args); err != nil {
			continue
		}

		// Check if the command modifies files
		if !isFileModifyingCommand(args.Cmd) {
			continue
		}

		// Extract file paths from the command
		extractedFiles := extractFilesFromCommand(args.Cmd, args.Workdir)
		for _, f := range extractedFiles {
			if f != "" && !fileSet[f] {
				fileSet[f] = true
				files = append(files, f)
			}
		}
	}

	return files
}

// isFileModifyingCommand checks if a shell command modifies files.
func isFileModifyingCommand(cmd string) bool {
	for _, pattern := range fileModifyingPatterns {
		if strings.Contains(cmd, pattern) {
			return true
		}
	}
	return false
}

// extractFilesFromCommand attempts to extract file paths from a shell command.
// This is heuristic-based — it handles common patterns but not all edge cases.
func extractFilesFromCommand(cmd, workdir string) []string {
	var files []string

	// Handle apply_patch — look for diff headers
	if strings.Contains(cmd, "apply_patch") {
		for _, line := range strings.Split(cmd, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "--- a/") || strings.HasPrefix(line, "+++ b/") {
				path := strings.TrimPrefix(line, "--- a/")
				path = strings.TrimPrefix(path, "+++ b/")
				if path != "" && path != "/dev/null" {
					if workdir != "" && !filepath.IsAbs(path) {
						path = filepath.Join(workdir, path)
					}
					files = append(files, path)
				}
			}
		}
		return files
	}

	// For other commands, extract paths from common patterns.
	// This is intentionally conservative — we only extract when
	// the pattern is unambiguous.

	// Handle "cat > file" or "tee file"
	if idx := strings.Index(cmd, " > "); idx > 0 {
		path := strings.TrimSpace(cmd[idx+3:])
		path = strings.SplitN(path, " ", 2)[0]
		path = strings.Trim(path, "'\"")
		if path != "" {
			files = append(files, path)
		}
	}

	// Handle "sed -i ... file" — last argument is typically the file
	if strings.Contains(cmd, "sed -i") {
		parts := strings.Fields(cmd)
		if len(parts) > 0 {
			last := parts[len(parts)-1]
			last = strings.Trim(last, "'\"")
			if !strings.HasPrefix(last, "-") && strings.Contains(last, ".") {
				files = append(files, last)
			}
		}
	}

	return files
}

// ExtractLastUserPrompt extracts the last user message from the transcript.
func ExtractLastUserPrompt(lines []TranscriptLine) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Type != eventTypeEventMsg {
			continue
		}

		var payload eventMsgPayload
		if err := json.Unmarshal(lines[i].Payload, &payload); err != nil {
			continue
		}

		if payload.Type == eventMsgUserMessage && payload.Message != "" {
			return payload.Message
		}
	}
	return ""
}

// ExtractAllUserPrompts collects all user messages from the transcript.
func ExtractAllUserPrompts(lines []TranscriptLine) []string {
	var prompts []string
	for _, line := range lines {
		if line.Type != eventTypeEventMsg {
			continue
		}

		var payload eventMsgPayload
		if err := json.Unmarshal(line.Payload, &payload); err != nil {
			continue
		}

		if payload.Type == eventMsgUserMessage && payload.Message != "" {
			prompts = append(prompts, payload.Message)
		}
	}
	return prompts
}

// ExtractLastAssistantMessage extracts the last agent message from the transcript.
func ExtractLastAssistantMessage(lines []TranscriptLine) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Type != eventTypeEventMsg {
			continue
		}

		var payload eventMsgPayload
		if err := json.Unmarshal(lines[i].Payload, &payload); err != nil {
			continue
		}

		if payload.Type == eventMsgAgentMessage && payload.Message != "" {
			return payload.Message
		}
	}
	return ""
}

// ExtractSessionID extracts the session ID from the first session_meta event.
func ExtractSessionID(lines []TranscriptLine) string {
	for _, line := range lines {
		if line.Type != eventTypeSessionMeta {
			continue
		}

		var meta sessionMetaPayload
		if err := json.Unmarshal(line.Payload, &meta); err != nil {
			continue
		}

		return meta.ID
	}
	return ""
}

// ExtractSessionCWD extracts the working directory from session metadata.
func ExtractSessionCWD(lines []TranscriptLine) string {
	for _, line := range lines {
		if line.Type != eventTypeSessionMeta {
			continue
		}

		var meta sessionMetaPayload
		if err := json.Unmarshal(line.Payload, &meta); err != nil {
			continue
		}

		return meta.CWD
	}
	return ""
}

// CalculateTokenUsage calculates token usage from a Codex transcript.
// Codex emits token_count events with cumulative totals per turn.
// We take the last token_count per turn to get accurate turn-level usage.
func CalculateTokenUsage(lines []TranscriptLine) *agent.TokenUsage {
	usage := &agent.TokenUsage{}

	// Track the last token_count event (cumulative for the session)
	var lastInfo tokenCountInfo

	for _, line := range lines {
		if line.Type != eventTypeEventMsg {
			continue
		}

		var payload eventMsgPayload
		if err := json.Unmarshal(line.Payload, &payload); err != nil {
			continue
		}

		if payload.Type != eventMsgTokenCount || payload.Info == nil {
			continue
		}

		var info tokenCountInfo
		if err := json.Unmarshal(payload.Info, &info); err != nil {
			continue
		}

		lastInfo = info
	}

	// Use the last cumulative token count
	usage.InputTokens = lastInfo.TotalTokenUsage.InputTokens
	usage.CacheReadTokens = lastInfo.TotalTokenUsage.CachedInputTokens
	usage.OutputTokens = lastInfo.TotalTokenUsage.OutputTokens

	// Count API calls by counting task_complete events
	for _, line := range lines {
		if line.Type != eventTypeEventMsg {
			continue
		}
		var payload eventMsgPayload
		if err := json.Unmarshal(line.Payload, &payload); err != nil {
			continue
		}
		if payload.Type == eventMsgTaskComplete {
			usage.APICallCount++
		}
	}

	return usage
}

// CalculateTokenUsageFromFile calculates token usage from a Codex transcript file.
// If startLine > 0, only considers lines from startLine onwards.
func CalculateTokenUsageFromFile(path string, startLine int) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}

	lines, err := parseTranscriptFromLine(path, startLine)
	if err != nil {
		return nil, err
	}

	return CalculateTokenUsage(lines), nil
}

// parseTranscriptFromLine parses a transcript file starting from a specific line.
func parseTranscriptFromLine(path string, startLine int) ([]TranscriptLine, error) {
	file, err := os.Open(path) //nolint:gosec // Path comes from Codex transcript location
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
