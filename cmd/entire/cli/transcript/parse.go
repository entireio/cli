package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/textutil"
)

// ParseFromBytes parses transcript content from a byte slice.
// Uses bufio.Reader to handle arbitrarily long lines.
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

		var line Line
		if err := json.Unmarshal(lineBytes, &line); err == nil {
			lines = append(lines, line)
		}

		if err == io.EOF {
			break
		}
	}

	return lines, nil
}

// ParseFromFileAtLine reads and parses a transcript file starting from a specific line.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns:
//   - lines: parsed transcript lines from startLine onwards (malformed lines skipped)
//   - totalLines: total number of lines in the file (including malformed ones)
//   - error: any error encountered during reading
//
// The startLine parameter is 0-indexed (startLine=0 reads from the beginning).
// This is useful for incremental parsing when you've already processed some lines.
func ParseFromFileAtLine(path string, startLine int) ([]Line, int, error) {
	file, err := os.Open(path) //nolint:gosec // path is a controlled transcript file path
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	var lines []Line
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
			var line Line
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
