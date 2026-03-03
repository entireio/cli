package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
)

// FileEditHookInput represents parsed input from the post-file-edit hook.
// Populated exclusively by parseFileEditHookInput.
type FileEditHookInput struct {
	SessionID    string
	ToolName     string
	ToolUseID    string
	FilePath     string
	LinesAdded   int
	LinesRemoved int
}

// fileEditToolInputWrite is the tool_input structure for the Write tool.
type fileEditToolInputWrite struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// fileEditToolInputEdit is the tool_input structure for the Edit tool.
type fileEditToolInputEdit struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// parseFileEditHookInput parses post-file-edit hook input from reader.
// Extracts session context, file path, tool info, and computes line counts from the tool payload.
// Uses SubagentCheckpointHookInput as the raw JSON shape (superset containing the needed fields).
func parseFileEditHookInput(r io.Reader) (*FileEditHookInput, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var raw SubagentCheckpointHookInput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	if raw.SessionID == "" {
		return nil, errors.New("missing session_id in hook input")
	}

	result := &FileEditHookInput{
		SessionID: raw.SessionID,
		ToolName:  raw.ToolName,
		ToolUseID: raw.ToolUseID,
	}

	switch raw.ToolName {
	case claudecode.ToolWrite:
		var input fileEditToolInputWrite
		if err := json.Unmarshal(raw.ToolInput, &input); err != nil {
			return nil, fmt.Errorf("failed to parse Write tool_input: %w", err)
		}
		if input.FilePath == "" {
			return nil, errors.New("missing file_path in Write tool_input")
		}
		result.FilePath = input.FilePath
		result.LinesAdded = agent.CountLines(input.Content)
		result.LinesRemoved = 0
	case claudecode.ToolEdit:
		var input fileEditToolInputEdit
		if err := json.Unmarshal(raw.ToolInput, &input); err != nil {
			return nil, fmt.Errorf("failed to parse Edit tool_input: %w", err)
		}
		if input.FilePath == "" {
			return nil, errors.New("missing file_path in Edit tool_input")
		}
		result.FilePath = input.FilePath
		result.LinesAdded = agent.CountLines(input.NewString)
		result.LinesRemoved = agent.CountLines(input.OldString)
	default:
		return nil, fmt.Errorf("unsupported tool: %s", raw.ToolName)
	}

	return result, nil
}
