package copilotcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Compile-time interface check.
var _ agent.TranscriptAnalyzer = (*CopilotCLIAgent)(nil)

// copilotEvent is a single line in events.jsonl.
type copilotEvent struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	ParentID  string          `json:"parentId"`
}

const (
	eventTypeUserMessage  = "user.message"
	eventTypeAssistantMsg = "assistant.message"
	eventTypeToolExecDone = "tool.execution_complete"
	eventTypeModelChange  = "session.model_change"
)

// userMessageData is the data payload for user.message events.
// IMPORTANT: The field is "content", not "message" — verified from real Copilot JSONL.
type userMessageData struct {
	Content string `json:"content"`
}

type assistantMessageData struct {
	Content string `json:"content"`
}

// modelChangeData is the data payload for session.model_change events.
// Copilot CLI emits this early in the transcript with the LLM model being used.
type modelChangeData struct {
	NewModel string `json:"newModel"`
}

type toolExecCompleteData struct {
	ToolCallID    string        `json:"toolCallId"`
	Model         string        `json:"model"`
	ToolTelemetry toolTelemetry `json:"toolTelemetry"`
}

type toolTelemetry struct {
	Metrics    toolMetrics    `json:"metrics"`
	Properties toolProperties `json:"properties"`
}

type toolMetrics struct {
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
}

// toolProperties contains string-encoded metadata from tool execution.
// filePaths is a JSON-encoded string array, e.g. "[\"path/to/file.txt\"]".
type toolProperties struct {
	FilePaths string `json:"filePaths"`
}

// parseEventsFromBytes scans JSONL data and returns all parsed events.
// Malformed lines are silently skipped.
func parseEventsFromBytes(data []byte) ([]copilotEvent, error) {
	return parseEventsFromOffset(data, 0)
}

// parseEventsFromOffset scans JSONL data and returns events starting after
// the first startOffset lines. Malformed lines are silently skipped.
func parseEventsFromOffset(data []byte, startOffset int) ([]copilotEvent, error) {
	var events []copilotEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 10*1024*1024) // 10 MB max line
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		if lineNum <= startOffset {
			continue
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev copilotEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip malformed lines
		}
		events = append(events, ev)
	}

	if err := scanner.Err(); err != nil {
		return events, fmt.Errorf("transcript scanner error: %w", err)
	}

	return events, nil
}

// extractModifiedFilesFromEvents collects file paths from tool.execution_complete
// events and returns a deduplicated list.
func extractModifiedFilesFromEvents(events []copilotEvent) []string {
	seen := make(map[string]bool)
	var files []string

	for i := range events {
		if events[i].Type != eventTypeToolExecDone {
			continue
		}

		var data toolExecCompleteData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		// filePaths is a JSON-encoded string array in properties, e.g. "[\"path/to/file\"]"
		if data.ToolTelemetry.Properties.FilePaths == "" {
			continue
		}
		var paths []string
		if err := json.Unmarshal([]byte(data.ToolTelemetry.Properties.FilePaths), &paths); err != nil {
			continue
		}
		for _, fp := range paths {
			if fp != "" && !seen[fp] {
				seen[fp] = true
				files = append(files, fp)
			}
		}
	}

	return files
}

// extractPromptsFromEvents collects content from user.message events.
func extractPromptsFromEvents(events []copilotEvent) []string {
	var prompts []string

	for i := range events {
		if events[i].Type != eventTypeUserMessage {
			continue
		}

		var data userMessageData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if data.Content != "" {
			prompts = append(prompts, data.Content)
		}
	}

	return prompts
}

// extractSummaryFromEvents returns the content of the last assistant.message event.
func extractSummaryFromEvents(events []copilotEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != eventTypeAssistantMsg {
			continue
		}

		var data assistantMessageData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if data.Content != "" {
			return data.Content
		}
	}

	return ""
}

// extractModelFromEvents returns the model from transcript events.
// First checks session.model_change events, then falls back to the model field
// in tool.execution_complete events (Copilot CLI includes model per tool call).
func extractModelFromEvents(events []copilotEvent) string {
	// Primary: session.model_change (explicit model declaration)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != eventTypeModelChange {
			continue
		}

		var data modelChangeData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if data.NewModel != "" {
			return data.NewModel
		}
	}

	// Fallback: tool.execution_complete events include a model field
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != eventTypeToolExecDone {
			continue
		}

		var data toolExecCompleteData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if data.Model != "" {
			return data.Model
		}
	}

	return ""
}

// ExtractModelFromTranscript extracts the LLM model name from a Copilot CLI
// transcript. It prefers session.model_change events (explicit model
// declarations), but falls back to the model field on tool.execution_complete
// events for Copilot CLI versions that do not emit session.model_change.
// Returns the last observed model, or empty string if unavailable.
func ExtractModelFromTranscript(ctx context.Context, transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}

	data, err := os.ReadFile(transcriptPath) //nolint:gosec // Path derived from agent hook input
	if err != nil {
		logging.Debug(ctx, "copilot-cli: failed to read transcript for model extraction",
			"transcriptPath", transcriptPath, "err", err)
		return ""
	}

	events, err := parseEventsFromBytes(data)
	if err != nil {
		logging.Debug(ctx, "copilot-cli: failed to parse transcript for model extraction",
			"transcriptPath", transcriptPath, "err", err)
		return ""
	}

	model := extractModelFromEvents(events)
	if model == "" {
		logging.Debug(ctx, "copilot-cli: no model found in transcript",
			"transcriptPath", transcriptPath, "eventCount", len(events))
	}

	return model
}

// GetTranscriptPosition returns the current line count of a Copilot CLI transcript.
// Copilot CLI uses JSONL format, so position is the number of lines.
// This is a lightweight operation that only counts lines without parsing JSON.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns 0 if the file doesn't exist or is empty.
func (c *CopilotCLIAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from Copilot CLI transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineCount := 0

	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if readErr == io.EOF {
				if len(line) > 0 {
					lineCount++ // Count final line without trailing newline
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}
		lineCount++
	}

	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line number.
// For Copilot CLI (JSONL format), offset is the starting line number.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns:
//   - files: list of file paths modified by Copilot (from tool.execution_complete events)
//   - currentPosition: total number of lines in the file
//   - error: any error encountered during reading
func (c *CopilotCLIAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from Copilot CLI transcript location
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var events []copilotEvent
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var ev copilotEvent
				if parseErr := json.Unmarshal(lineData, &ev); parseErr == nil {
					events = append(events, ev)
				}
				// Skip malformed lines silently
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return extractModifiedFilesFromEvents(events), lineNum, nil
}

// ExtractPrompts extracts user prompts from the transcript starting at the given offset.
func (c *CopilotCLIAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	events, err := parseEventsFromOffset(data, fromOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript events: %w", err)
	}
	return extractPromptsFromEvents(events), nil
}

// ExtractSummary extracts the last assistant message as a session summary.
func (c *CopilotCLIAgent) ExtractSummary(sessionRef string) (string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}

	events, err := parseEventsFromBytes(data)
	if err != nil {
		return "", fmt.Errorf("failed to parse transcript events: %w", err)
	}
	return extractSummaryFromEvents(events), nil
}
