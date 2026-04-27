package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions.
var (
	_ agent.TranscriptAnalyzer          = (*CodexAgent)(nil)
	_ agent.TokenCalculator             = (*CodexAgent)(nil)
	_ agent.PromptExtractor             = (*CodexAgent)(nil)
	_ agent.RestoredSessionPathResolver = (*CodexAgent)(nil)
)

// rolloutLine is the top-level JSONL line structure in Codex rollout files.
type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // "session_meta", "response_item", "event_msg", "turn_context"
	Payload   json.RawMessage `json:"payload"`
}

const rolloutLineTypeResponseItem = "response_item"

// sessionMetaPayload is the payload for type="session_meta" lines.
type sessionMetaPayload struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
}

// responseItemPayload is the payload for type="response_item" lines.
type responseItemPayload struct {
	Type    string          `json:"type"` // "message", "custom_tool_call", "custom_tool_call_output", "local_shell_call", "function_call", etc.
	Role    string          `json:"role,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   string          `json:"input,omitempty"`   // apply_patch input (plain text, not JSON)
	Content json.RawMessage `json:"content,omitempty"` // for messages
}

// contentItem is a single content block in a message.
type contentItem struct {
	Type string `json:"type"` // "input_text", "output_text"
	Text string `json:"text"`
}

// eventMsgPayload is the payload for type="event_msg" lines.
type eventMsgPayload struct {
	Type string          `json:"type"` // "token_count", "task_started", "user_message", "agent_message", "task_complete"
	Info json.RawMessage `json:"info,omitempty"`
}

// tokenCountInfo contains token usage data from event_msg.token_count.
type tokenCountInfo struct {
	TotalTokenUsage *tokenUsageData `json:"total_token_usage,omitempty"`
}

// tokenUsageData maps to Codex's token usage fields.
type tokenUsageData struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// applyPatchFileRegex extracts file paths from apply_patch input.
// Matches "*** Add File: <path>", "*** Update File: <path>", "*** Delete File: <path>"
var applyPatchFileRegex = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: (.+)`)

// GetTranscriptPosition returns the current line count of a Codex rollout transcript.
func (c *CodexAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineCount := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if readErr == io.EOF {
				if len(line) > 0 {
					lineCount++
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}
		lineCount++
	}
	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line offset.
func (c *CodexAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from agent hook input
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	seen := make(map[string]struct{})
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				for _, f := range extractFilesFromLine(lineData) {
					if _, ok := seen[f]; !ok {
						seen[f] = struct{}{}
						files = append(files, f)
					}
				}
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return files, lineNum, nil
}

// extractFilesFromLine extracts modified file paths from a single rollout JSONL line.
func extractFilesFromLine(lineData []byte) []string {
	var line rolloutLine
	if json.Unmarshal(lineData, &line) != nil {
		return nil
	}

	if line.Type != rolloutLineTypeResponseItem {
		return nil
	}

	var payload responseItemPayload
	if json.Unmarshal(line.Payload, &payload) != nil {
		return nil
	}

	// apply_patch custom tool calls contain file paths in the input text
	if payload.Type == "custom_tool_call" && payload.Name == "apply_patch" {
		return extractFilesFromApplyPatch(payload.Input)
	}

	return nil
}

// extractFilesFromApplyPatch parses apply_patch input for file paths.
// Format: "*** Add File: <path>" or "*** Update File: <path>" or "*** Delete File: <path>"
func extractFilesFromApplyPatch(input string) []string {
	matches := applyPatchFileRegex.FindAllStringSubmatch(input, -1)
	var files []string
	seen := make(map[string]struct{})
	for _, m := range matches {
		path := strings.TrimSpace(m[1])
		if path != "" {
			if _, ok := seen[path]; !ok {
				seen[path] = struct{}{}
				files = append(files, path)
			}
		}
	}
	return files
}

// CalculateTokenUsage computes token usage from the transcript starting at the given line offset.
// Codex reports cumulative total_token_usage, so we compute the delta between the last
// token_count at/before the offset and the last token_count after the offset.
func (c *CodexAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	var baselineUsage *tokenUsageData // last token_count at or before offset
	var lastUsage *tokenUsageData     // last token_count after offset
	apiCalls := 0
	lineNum := 0

	for _, lineData := range splitJSONL(transcriptData) {
		lineNum++

		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil {
			continue
		}
		if line.Type != "event_msg" {
			continue
		}
		var evt eventMsgPayload
		if json.Unmarshal(line.Payload, &evt) != nil {
			continue
		}
		if evt.Type != "token_count" || len(evt.Info) == 0 {
			continue
		}
		var info tokenCountInfo
		if json.Unmarshal(evt.Info, &info) != nil || info.TotalTokenUsage == nil {
			continue
		}

		if lineNum <= fromOffset {
			baselineUsage = info.TotalTokenUsage
		} else {
			lastUsage = info.TotalTokenUsage
			apiCalls++
		}
	}

	if lastUsage == nil {
		return nil, nil //nolint:nilnil // no usage data found
	}

	// Subtract baseline to get the delta for this checkpoint range
	inputTokens := lastUsage.InputTokens
	cacheReadTokens := lastUsage.CachedInputTokens
	outputTokens := lastUsage.OutputTokens
	if baselineUsage != nil {
		inputTokens -= baselineUsage.InputTokens
		cacheReadTokens -= baselineUsage.CachedInputTokens
		outputTokens -= baselineUsage.OutputTokens
	}

	freshInputTokens := inputTokens - cacheReadTokens
	if freshInputTokens < 0 {
		freshInputTokens = 0
	}

	return &agent.TokenUsage{
		InputTokens:     freshInputTokens,
		CacheReadTokens: cacheReadTokens,
		OutputTokens:    outputTokens,
		APICallCount:    apiCalls,
	}, nil
}

// ExtractPrompts returns user prompts from the transcript starting at the given offset.
func (c *CodexAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	var prompts []string
	lineNum := 0

	for _, lineData := range splitJSONL(data) {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}

		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil {
			continue
		}

		if line.Type != rolloutLineTypeResponseItem {
			continue
		}

		var payload responseItemPayload
		if json.Unmarshal(line.Payload, &payload) != nil {
			continue
		}

		if payload.Type != "message" || payload.Role != "user" {
			continue
		}

		// Extract text from content items
		var items []contentItem
		if json.Unmarshal(payload.Content, &items) != nil {
			continue
		}
		for _, item := range items {
			text := strings.TrimSpace(item.Text)
			if text != "" && item.Type == "input_text" {
				prompts = append(prompts, text)
			}
		}
	}

	return prompts, nil
}

// SanitizePortableTranscript strips encrypted history fragments that cannot be
// replayed when Entire reconstructs a Codex rollout outside its original
// session context.
func SanitizePortableTranscript(data []byte) []byte {
	lines := splitJSONL(data)
	if len(lines) == 0 {
		return data
	}

	sanitized := make([][]byte, 0, len(lines))
	for _, lineData := range lines {
		updated, keep := sanitizeRolloutLine(lineData)
		if !keep {
			continue
		}
		sanitized = append(sanitized, updated)
	}

	if len(sanitized) == 0 {
		return data
	}
	return agent.ReassembleJSONL(sanitized)
}

func sanitizeRestoredTranscript(data []byte) []byte {
	return SanitizePortableTranscript(data)
}

func sanitizeRolloutLine(lineData []byte) ([]byte, bool) {
	var line rolloutLine
	if err := json.Unmarshal(lineData, &line); err != nil {
		return lineData, true
	}
	if line.Type == "compacted" {
		return sanitizeCompactedLine(line)
	}
	if line.Type != rolloutLineTypeResponseItem {
		return lineData, true
	}

	var payload map[string]any
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		return lineData, true
	}

	itemType, ok := payload["type"].(string)
	if !ok {
		return lineData, true
	}
	switch itemType {
	case "reasoning":
		delete(payload, "encrypted_content")
	case "compaction", "compaction_summary":
		return nil, false
	default:
		return lineData, true
	}

	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return lineData, true
	}
	line.Payload = encodedPayload

	encodedLine, err := json.Marshal(line)
	if err != nil {
		return lineData, true
	}
	return encodedLine, true
}

func sanitizeCompactedLine(line rolloutLine) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		return mustMarshalRolloutLine(line), true
	}

	replacementHistory, ok := payload["replacement_history"].([]any)
	if !ok {
		return mustMarshalRolloutLine(line), true
	}

	sanitizedHistory := sanitizeHistoryItems(replacementHistory)
	payload["replacement_history"] = sanitizedHistory

	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return mustMarshalRolloutLine(line), true
	}
	line.Payload = encodedPayload

	return mustMarshalRolloutLine(line), true
}

func sanitizeHistoryItems(items []any) []any {
	sanitized := make([]any, 0, len(items))
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			sanitized = append(sanitized, item)
			continue
		}

		itemType, ok := itemMap["type"].(string)
		if !ok {
			sanitized = append(sanitized, itemMap)
			continue
		}
		switch itemType {
		case "reasoning":
			delete(itemMap, "encrypted_content")
		case "compaction", "compaction_summary":
			continue
		}

		sanitized = append(sanitized, itemMap)
	}
	return sanitized
}

func mustMarshalRolloutLine(line rolloutLine) []byte {
	encodedLine, err := json.Marshal(line)
	if err != nil {
		return nil
	}
	return encodedLine
}

// splitJSONL splits JSONL bytes into individual lines, skipping empty lines.
func splitJSONL(data []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseSessionStartTime(data []byte) (time.Time, error) {
	lines := splitJSONL(data)
	if len(lines) == 0 {
		return time.Time{}, errors.New("transcript is empty")
	}

	var line rolloutLine
	if err := json.Unmarshal(lines[0], &line); err != nil {
		return time.Time{}, fmt.Errorf("parse first transcript line: %w", err)
	}
	if line.Type != "session_meta" {
		return time.Time{}, fmt.Errorf("first transcript line is %q, want session_meta", line.Type)
	}

	var meta sessionMetaPayload
	if err := json.Unmarshal(line.Payload, &meta); err != nil {
		return time.Time{}, fmt.Errorf("parse session_meta payload: %w", err)
	}
	if meta.Timestamp == "" {
		return time.Time{}, errors.New("session_meta timestamp is empty")
	}

	startTime, err := time.Parse(time.RFC3339Nano, meta.Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse session_meta timestamp %q: %w", meta.Timestamp, err)
	}
	return startTime, nil
}
