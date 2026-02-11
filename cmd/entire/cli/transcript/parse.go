package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/textutil"
)

// ParseFromBytes parses transcript content from a byte slice.
// Uses bufio.Reader to handle arbitrarily long lines.
//
// Supports:
//   - Claude/Gemini normalized JSONL lines with top-level type=user|assistant
//   - Pi JSONL message entries (type=message with message.role=user|assistant)
func ParseFromBytes(content []byte) ([]Line, error) {
	var lines []Line
	reader := bufio.NewReader(bytes.NewReader(content))

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

		if line, ok := parseTranscriptLine(lineBytes); ok {
			lines = append(lines, line)
		}

		if err == io.EOF {
			break
		}
	}

	return lines, nil
}

func parseTranscriptLine(lineBytes []byte) (Line, bool) {
	trimmed := bytes.TrimSpace(lineBytes)
	if len(trimmed) == 0 {
		return Line{}, false
	}

	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &base); err != nil {
		return Line{}, false
	}

	switch base.Type {
	case TypeUser, TypeAssistant:
		var line Line
		if err := json.Unmarshal(trimmed, &line); err != nil {
			return Line{}, false
		}
		return line, true
	case "message":
		return parsePiMessageLine(trimmed)
	default:
		return Line{}, false
	}
}

func parsePiMessageLine(lineBytes []byte) (Line, bool) {
	var piEntry struct {
		ID      string          `json:"id"`
		UUID    string          `json:"uuid"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(lineBytes, &piEntry); err != nil {
		return Line{}, false
	}
	if len(piEntry.Message) == 0 {
		return Line{}, false
	}

	var msg struct {
		Role    string      `json:"role"`
		Content interface{} `json:"content"`
	}
	if err := json.Unmarshal(piEntry.Message, &msg); err != nil {
		return Line{}, false
	}

	uuid := piEntry.ID
	if uuid == "" {
		uuid = piEntry.UUID
	}

	switch msg.Role {
	case "user":
		raw, err := json.Marshal(map[string]interface{}{"content": msg.Content})
		if err != nil {
			return Line{}, false
		}
		return Line{Type: TypeUser, UUID: uuid, Message: raw}, true
	case "assistant":
		normalizedContent := normalizePiAssistantContent(msg.Content)
		if len(normalizedContent) == 0 {
			return Line{}, false
		}
		raw, err := json.Marshal(map[string]interface{}{"content": normalizedContent})
		if err != nil {
			return Line{}, false
		}
		return Line{Type: TypeAssistant, UUID: uuid, Message: raw}, true
	default:
		return Line{}, false
	}
}

func normalizePiAssistantContent(content interface{}) []map[string]interface{} {
	blocks := make([]map[string]interface{}, 0)

	switch typed := content.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed != "" {
			blocks = append(blocks, map[string]interface{}{"type": ContentTypeText, "text": trimmed})
		}
	case []interface{}:
		for _, item := range typed {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}

			blockTypeValue, hasBlockType := m["type"]
			if !hasBlockType {
				continue
			}
			blockType, ok := blockTypeValue.(string)
			if !ok {
				continue
			}

			switch blockType {
			case ContentTypeText:
				textValue, hasText := m["text"]
				if !hasText {
					continue
				}
				text, ok := textValue.(string)
				if !ok {
					continue
				}

				trimmed := strings.TrimSpace(text)
				if trimmed == "" {
					continue
				}
				blocks = append(blocks, map[string]interface{}{"type": ContentTypeText, "text": trimmed})
			case "toolCall":
				nameValue, hasName := m["name"]
				if !hasName {
					continue
				}
				name, ok := nameValue.(string)
				if !ok || name == "" {
					continue
				}

				input := map[string]interface{}{}
				if args, ok := m["arguments"].(map[string]interface{}); ok {
					input = canonicalizePiToolCallInput(args)
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  ContentTypeToolUse,
					"name":  name,
					"input": input,
				})
			}
		}
	}

	return blocks
}

func canonicalizePiToolCallInput(args map[string]interface{}) map[string]interface{} {
	if len(args) == 0 {
		return map[string]interface{}{}
	}

	normalized := make(map[string]interface{}, len(args)+1)
	for key, value := range args {
		normalized[key] = value
	}

	if _, exists := normalized["file_path"]; !exists {
		if path, ok := normalized["path"].(string); ok && strings.TrimSpace(path) != "" {
			normalized["file_path"] = path
		}
	}

	return normalized
}

// SliceFromLine returns the content starting from line number `startLine` (0-indexed).
// This is used to extract only the checkpoint-specific portion of a cumulative transcript.
// For example, if startLine is 2, lines 0 and 1 are skipped and the result starts at line 2.
// Returns empty slice if startLine exceeds the number of lines.
func SliceFromLine(content []byte, startLine int) []byte {
	if len(content) == 0 || startLine <= 0 {
		return content
	}

	// Find the byte offset where startLine begins
	lineCount := 0
	offset := 0
	for i, b := range content {
		if b == '\n' {
			lineCount++
			if lineCount == startLine {
				offset = i + 1
				break
			}
		}
	}

	// If we didn't find enough lines, return empty
	if lineCount < startLine {
		return nil
	}

	// If offset is beyond content, return empty
	if offset >= len(content) {
		return nil
	}

	return content[offset:]
}

// ExtractUserContent extracts user content from a raw message.
// Handles both string and array content formats.
// IDE-injected context tags (like <ide_opened_file>) are stripped from the result.
// Returns empty string if the message cannot be parsed or contains no text.
func ExtractUserContent(message json.RawMessage) string {
	var msg UserMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return ""
	}

	// Handle string content
	if str, ok := msg.Content.(string); ok {
		return textutil.StripIDEContextTags(str)
	}

	// Handle array content (only if it contains text blocks)
	if arr, ok := msg.Content.([]interface{}); ok {
		var texts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == ContentTypeText {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		if len(texts) > 0 {
			return textutil.StripIDEContextTags(strings.Join(texts, "\n\n"))
		}
	}

	return ""
}
