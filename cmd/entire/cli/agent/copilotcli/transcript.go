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

// Compile-time interface checks.
var (
	_ agent.TranscriptAnalyzer = (*CopilotCLIAgent)(nil)
	_ agent.TokenCalculator    = (*CopilotCLIAgent)(nil)
)

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

// lastEventField scans events newest-first for entries of eventType,
// returning the first non-empty value extract produces (skipping entries
// whose data doesn't unmarshal).
func lastEventField[T any](events []copilotEvent, eventType string, extract func(T) string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != eventType {
			continue
		}

		var data T
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if v := extract(data); v != "" {
			return v
		}
	}

	return ""
}

// extractSummaryFromEvents returns the content of the last assistant.message event.
func extractSummaryFromEvents(events []copilotEvent) string {
	return lastEventField(events, eventTypeAssistantMsg,
		func(d assistantMessageData) string { return d.Content })
}

// extractModelFromEvents returns the model from transcript events.
// First checks session.model_change events, then falls back to the model field
// in tool.execution_complete events (Copilot CLI includes model per tool call).
func extractModelFromEvents(events []copilotEvent) string {
	if model := lastEventField(events, eventTypeModelChange,
		func(d modelChangeData) string { return d.NewModel }); model != "" {
		return model
	}
	return lastEventField(events, eventTypeToolExecDone,
		func(d toolExecCompleteData) string { return d.Model })
}

// sessionShutdownData is the data payload for session.shutdown events.
// Contains aggregate model metrics for the entire session.
// modelMetrics is a JSON object keyed by model name (e.g. "claude-sonnet-4.6"),
// not an array — using map[string] here matches the real Copilot CLI wire format.
type sessionShutdownData struct {
	ModelMetrics map[string]modelMetric `json:"modelMetrics"`
}

// modelMetric contains per-model token usage aggregates.
// The model name is the map key in sessionShutdownData.ModelMetrics, not a field.
type modelMetric struct {
	Requests modelRequests `json:"requests"`
	Usage    modelUsage    `json:"usage"`
}

type modelRequests struct {
	Count int `json:"count"`
}

type modelUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

// assistantMessageTokenData is used as a fallback when session.shutdown is not
// yet available. Each assistant.message may carry an outputTokens field.
type assistantMessageTokenData struct {
	OutputTokens int `json:"outputTokens"`
}

const eventTypeSessionShutdown = "session.shutdown"

// CalculateTokenUsage computes token usage from the Copilot CLI JSONL transcript.
// For full transcripts (fromOffset == 0), it prefers session.shutdown, which
// contains session-wide aggregates. For sliced transcripts, session.shutdown
// would overcount because it is not checkpoint-scoped, so we fall back to
// summing assistant.message outputTokens within the slice.
func (c *CopilotCLIAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	events, err := parseEventsFromOffset(transcriptData, fromOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	return extractTokenUsageFromEvents(events, fromOffset == 0), nil
}

// extractTokenUsageFromEvents extracts token usage from parsed events.
// Prefers session.shutdown aggregate only when the caller is looking at the
// full transcript; otherwise falls back to per-message outputTokens.
func extractTokenUsageFromEvents(events []copilotEvent, preferSessionShutdown bool) *agent.TokenUsage {
	if preferSessionShutdown {
		// session.shutdown is authoritative, but only for full-session totals.
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type != eventTypeSessionShutdown {
				continue
			}

			var data sessionShutdownData
			if err := json.Unmarshal(events[i].Data, &data); err != nil {
				logging.Debug(context.Background(), "copilot-cli: session.shutdown data unmarshal failed",
					"err", err)
				continue
			}

			if len(data.ModelMetrics) == 0 {
				continue
			}

			// Sum across all models.
			usage := &agent.TokenUsage{}
			for _, m := range data.ModelMetrics {
				usage.InputTokens += m.Usage.InputTokens
				usage.OutputTokens += m.Usage.OutputTokens
				usage.CacheReadTokens += m.Usage.CacheReadTokens
				usage.CacheCreationTokens += m.Usage.CacheWriteTokens
				usage.APICallCount += m.Requests.Count
			}
			return usage
		}
	}

	// Fallback: sum outputTokens from assistant.message events
	usage := &agent.TokenUsage{}
	for i := range events {
		if events[i].Type != eventTypeAssistantMsg {
			continue
		}

		var data assistantMessageTokenData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}
		usage.OutputTokens += data.OutputTokens
		if data.OutputTokens > 0 {
			usage.APICallCount++
		}
	}

	return usage
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
