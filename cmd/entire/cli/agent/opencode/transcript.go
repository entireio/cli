package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// TranscriptLine is an alias to the shared transcript.Line type.
type TranscriptLine = transcript.Line

// Message role constants.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Part type constants.
const (
	PartTypeText = "text"
	PartTypeTool = "tool"
)

// OpenCode export format structures.
type exportData struct {
	Info struct {
		ID string `json:"id"`
	} `json:"info"`
	Messages []exportMessage `json:"messages"`
}

type exportMessage struct {
	Info struct {
		ID   string `json:"id"`
		Role string `json:"role"`
		Time struct {
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
		} `json:"tokens,omitempty"`
		ModelID    string `json:"modelID,omitempty"`
		ProviderID string `json:"providerID,omitempty"`
	} `json:"info"`
	Parts []exportPart `json:"parts"`
}

type exportPart struct {
	Type string `json:"type"`
	// For text type
	Text string `json:"text,omitempty"`
	// For tool type
	Tool  string           `json:"tool,omitempty"`
	State *exportToolState `json:"state,omitempty"`
}

type exportToolState struct {
	Input  map[string]interface{} `json:"input,omitempty"`
	Output interface{}            `json:"output,omitempty"`
	Status string                 `json:"status,omitempty"`
}

// ExportSession runs `opencode export <sessionID>` from the repository root.
func ExportSession(sessionID string) ([]byte, error) {
	ctx := context.Background()
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return nil, fmt.Errorf("failed to get repo root: %w", err)
	}

	cmd := exec.CommandContext(ctx, "opencode", "export", sessionID)
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode export failed: %w", err)
	}
	return output, nil
}

// TranscriptLineWithTime represents a transcript line with its creation timestamp.
type TranscriptLineWithTime struct {
	Line      TranscriptLine
	CreatedAt time.Time
}

// ConvertExportToJSONL converts OpenCode export JSON to JSONL format compatible with Entire.
func ConvertExportToJSONL(exportJSON []byte) ([]byte, error) {
	var data exportData
	if err := json.Unmarshal(exportJSON, &data); err != nil {
		return nil, fmt.Errorf("failed to parse export data: %w", err)
	}

	var lines []TranscriptLineWithTime
	for _, msg := range data.Messages {
		line, err := convertMessageToLine(msg)
		if err != nil {
			continue // Skip malformed messages
		}
		createdAt := time.Now()
		if msg.Info.Time.Created > 0 {
			createdAt = time.Unix(msg.Info.Time.Created, 0)
		}
		lines = append(lines, TranscriptLineWithTime{
			Line:      line,
			CreatedAt: createdAt,
		})
	}

	return SerializeTranscriptWithTime(lines)
}

// convertMessageToLine converts an OpenCode export message to a transcript line.
func convertMessageToLine(msg exportMessage) (TranscriptLine, error) {
	var messageContent interface{}

	switch msg.Info.Role {
	case RoleUser:
		// Extract text content from parts
		var textContent string
		for _, part := range msg.Parts {
			if part.Type == PartTypeText && part.Text != "" {
				textContent = part.Text
				break
			}
		}
		messageContent = transcript.UserMessage{
			Content: textContent,
		}

	case RoleAssistant:
		// Build content blocks from parts
		var contentBlocks []transcript.ContentBlock
		for _, part := range msg.Parts {
			switch part.Type {
			case PartTypeText:
				if part.Text != "" {
					contentBlocks = append(contentBlocks, transcript.ContentBlock{
						Type: PartTypeText,
						Text: part.Text,
					})
				}
			case PartTypeTool:
				if part.Tool != "" && part.State != nil {
					inputJSON, err := json.Marshal(part.State.Input)
					if err != nil {
						// Skip tool calls with non-serializable input
						continue
					}
					contentBlocks = append(contentBlocks, transcript.ContentBlock{
						Type:  "tool_use",
						Name:  part.Tool,
						Input: inputJSON,
					})
				}
			}
		}

		messageContent = map[string]interface{}{
			"content": contentBlocks,
			"id":      msg.Info.ID,
			"model":   msg.Info.ModelID,
			"usage": map[string]interface{}{
				"input_tokens":                msg.Info.Tokens.Input,
				"output_tokens":               msg.Info.Tokens.Output,
				"cache_creation_input_tokens": msg.Info.Tokens.Cache.Write,
				"cache_read_input_tokens":     msg.Info.Tokens.Cache.Read,
			},
		}

	default:
		// Skip unknown roles (system, tool, etc.)
		return TranscriptLine{}, fmt.Errorf("unknown message role: %s", msg.Info.Role)
	}

	msgJSON, err := json.Marshal(messageContent)
	if err != nil {
		return TranscriptLine{}, fmt.Errorf("failed to marshal message: %w", err)
	}

	return TranscriptLine{
		Type:    msg.Info.Role,
		UUID:    msg.Info.ID,
		Message: msgJSON,
	}, nil
}

// ReconstructTranscript exports and converts an OpenCode session to JSONL format.
func ReconstructTranscript(sessionID string) ([]byte, error) {
	exportJSON, err := ExportSession(sessionID)
	if err != nil {
		return nil, err
	}
	return ConvertExportToJSONL(exportJSON)
}

// SerializeTranscript converts transcript lines to JSONL bytes.
func SerializeTranscript(lines []TranscriptLine) ([]byte, error) {
	var linesWithTime []TranscriptLineWithTime
	for _, line := range lines {
		linesWithTime = append(linesWithTime, TranscriptLineWithTime{
			Line:      line,
			CreatedAt: time.Now(),
		})
	}
	return SerializeTranscriptWithTime(linesWithTime)
}

// SerializeTranscriptWithTime converts transcript lines with timestamps to JSONL bytes.
func SerializeTranscriptWithTime(lines []TranscriptLineWithTime) ([]byte, error) {
	var buf bytes.Buffer
	for _, line := range lines {
		lineWithTimestamp := struct {
			Type      string          `json:"type"`
			UUID      string          `json:"uuid"`
			Message   json.RawMessage `json:"message"`
			Timestamp string          `json:"timestamp"`
		}{
			Type:      line.Line.Type,
			UUID:      line.Line.UUID,
			Message:   line.Line.Message,
			Timestamp: line.CreatedAt.Format(time.RFC3339),
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
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
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

		if str, ok := msg.Content.(string); ok {
			return str
		}

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

		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type != "tool_use" {
				continue
			}

			if !isFileModificationTool(block.Name) {
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

// FileModificationTools lists OpenCode tools that modify files.
var FileModificationTools = []string{
	"write",
	"edit",
	"bash",
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
	seenMessages := make(map[string]bool)

	for _, line := range lines {
		if line.Type != "assistant" {
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		msgID, ok := msg["id"].(string)
		if !ok || msgID == "" || seenMessages[msgID] {
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
