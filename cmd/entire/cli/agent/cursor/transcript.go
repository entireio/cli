package cursor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
		// ExtractUserContent already strips IDE tags; stripping again is not a
		// no-op now that the <timestamp> strip is position-anchored.
		content := transcript.ExtractUserContent(lines[i].Message)
		if content != "" {
			prompts = append(prompts, content)
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

// ExtractModifiedFiles extracts the files modified by Cursor tool calls in the
// given transcript lines, in first-seen order and deduplicated.
//
// Cursor records tool_use content blocks in the same shape as Claude Code
// (message.content[i].type == "tool_use", with name and input), so this mirrors
// claudecode.ExtractModifiedFiles. The divergences are the tool names and the
// input key — see FileModificationTools and toolInput.
func ExtractModifiedFiles(lines []transcript.Line) []string {
	seen := make(map[string]bool)
	var files []string

	for i := range lines {
		if lines[i].Type != transcript.TypeAssistant {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(lines[i].Message, &msg); err != nil {
			continue
		}
		for _, block := range msg.Content {
			if block.Type != transcript.ContentTypeToolUse {
				continue
			}
			if !slices.Contains(FileModificationTools, block.Name) {
				continue
			}
			var input toolInput
			if err := json.Unmarshal(block.Input, &input); err != nil {
				continue
			}
			if input.Path == "" || seen[input.Path] {
				continue
			}
			seen[input.Path] = true
			files = append(files, input.Path)
		}
	}
	return files
}

// ExtractModifiedFilesFromOffset extracts files modified by tool calls appearing at
// or after startOffset, and returns the transcript's current line count.
//
// A missing transcript is not an error: Cursor reports transcript_path as null in
// CLI mode, so ResolveSessionFile predicts a path that may not exist yet, and this
// runs on capture paths that must fail open (matching GetTranscriptPosition).
func (c *CursorAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	if path == "" {
		return nil, 0, nil
	}

	lines, total, err := transcript.ParseFromFileAtLineWithTotal(path, startOffset)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to parse transcript: %w", err)
	}

	return ExtractModifiedFiles(lines), total, nil
}
