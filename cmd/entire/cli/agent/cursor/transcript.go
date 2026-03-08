package cursor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface assertion.
var _ agent.TranscriptAnalyzer = (*CursorAgent)(nil)

// GetTranscriptPosition returns the current line count of a Cursor transcript.
// Cursor uses the same JSONL format as Claude Code, so position is the number of lines.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns 0 if the file doesn't exist or is empty.
func (c *CursorAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from Cursor transcript location
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
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					lineCount++ // Count final line without trailing newline
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", err)
		}
		lineCount++
	}

	return lineCount, nil
}

// ExtractPrompts extracts user prompts from the transcript starting at the given line offset.
// Cursor uses the same JSONL format as Claude Code; the shared transcript package normalizes
// "role" → "type" and strips <user_query> tags.
func (c *CursorAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	lines, err := transcript.ParseFromFileAtLine(sessionRef, fromOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	var prompts []string
	for i := range lines {
		if lines[i].Type != transcript.TypeUser {
			continue
		}
		content := transcript.ExtractUserContent(lines[i].Message)
		if content != "" {
			prompts = append(prompts, textutil.StripIDEContextTags(content))
		}
	}
	return prompts, nil
}

// ExtractSummary extracts the last assistant message as a session summary.
func (c *CursorAgent) ExtractSummary(sessionRef string) (string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}

	lines, parseErr := transcript.ParseFromBytes(data)
	if parseErr != nil {
		return "", fmt.Errorf("failed to parse transcript: %w", parseErr)
	}

	// Walk backward to find last assistant text block
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Type != transcript.TypeAssistant {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(lines[i].Message, &msg); err != nil {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == transcript.ContentTypeText && block.Text != "" {
				return block.Text, nil
			}
		}
	}
	return "", nil
}

// ExtractModifiedFilesFromOffset returns no modified files for Cursor and a constant
// position of 0, because Cursor transcripts do not contain tool_use blocks.
// File detection relies on git status and hook-provided modified_files instead.
func (c *CursorAgent) ExtractModifiedFilesFromOffset(_ string, _ int) ([]string, int, error) {
	return nil, 0, nil
}
