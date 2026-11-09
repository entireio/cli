package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	transcriptlib "github.com/entireio/cli/transcript"
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
			normalizeLineType(&line)
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
//   - error: any error encountered during reading
//
// The startLine parameter is 0-indexed (startLine=0 reads from the beginning).
// This is useful for incremental parsing when you've already processed some lines.
func ParseFromFileAtLine(path string, startLine int) ([]Line, error) {
	lines, _, err := ParseFromFileAtLineWithTotal(path, startLine)
	return lines, err
}

// ParseFromFileAtLineWithTotal is ParseFromFileAtLine plus the file's total line
// count, gathered in the same pass.
//
// Callers implementing agent.TranscriptAnalyzer.ExtractModifiedFilesFromOffset need
// both halves at once: the slice from startLine, and the new position to record for
// the next call. Reading the file twice (parse, then count) is the thing to avoid —
// these run on hook paths where transcripts reach tens of megabytes.
//
// The total counts every non-empty line in the file, including lines before
// startLine and lines too malformed to unmarshal, so it stays a faithful position
// marker for a subsequent startLine.
func ParseFromFileAtLineWithTotal(path string, startLine int) ([]Line, int, error) {
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
				normalizeLineType(&line)
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

// normalizeLineType ensures line.Type is populated for all transcript formats.
// Claude Code uses "type" while Cursor uses "role" for the same purpose.
// When Type is empty but Role is set, we copy Role into Type so all downstream
// consumers can switch on Type uniformly.
func normalizeLineType(line *Line) {
	if line.Type == "" && line.Role != "" {
		line.Type = line.Role
	}
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
		return transcriptlib.StripIDEContextTags(str)
	}

	// Handle array content (only if it contains text blocks)
	if arr, ok := msg.Content.([]interface{}); ok {
		var texts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == transcriptlib.ContentTypeText {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		if len(texts) > 0 {
			return transcriptlib.StripIDEContextTags(strings.Join(texts, "\n\n"))
		}
	}

	return ""
}
