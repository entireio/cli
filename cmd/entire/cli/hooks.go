package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// SubagentCheckpointHookInput represents the JSON input from PostToolUse hooks for
// subagent checkpoint creation (TodoWrite, Edit, Write)
type SubagentCheckpointHookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

// parseSubagentCheckpointHookInput parses PostToolUse hook input for subagent checkpoints
func parseSubagentCheckpointHookInput(r io.Reader) (*SubagentCheckpointHookInput, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var input SubagentCheckpointHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &input, nil
}

// taskToolInput represents the tool_input structure for the Task tool.
// Used to extract subagent_type and description for descriptive commit messages.
type taskToolInput struct {
	SubagentType string `json:"subagent_type"`
	Description  string `json:"description"`
}

// ParseSubagentTypeAndDescription extracts subagent_type and description from Task tool_input.
// Returns empty strings if parsing fails or fields are not present.
func ParseSubagentTypeAndDescription(toolInput json.RawMessage) (agentType, description string) {
	if len(toolInput) == 0 {
		return "", ""
	}

	var input taskToolInput
	if err := json.Unmarshal(toolInput, &input); err != nil {
		return "", ""
	}

	return input.SubagentType, input.Description
}

// todoWriteToolInput represents the tool_input structure for the TodoWrite tool.
// Used to extract the todos array which is then passed to strategy.ExtractInProgressTodo.
type todoWriteToolInput struct {
	Todos json.RawMessage `json:"todos"`
}

// ExtractTodoContentFromToolInput extracts the content of the in-progress todo item from TodoWrite tool_input.
// Falls back to the first pending item if no in-progress item is found.
// Returns empty string if no suitable item is found or JSON is invalid.
//
// This function unwraps the outer tool_input object to extract the todos array,
// then delegates to strategy.ExtractInProgressTodo for the actual parsing logic.
func ExtractTodoContentFromToolInput(toolInput json.RawMessage) string {
	if len(toolInput) == 0 {
		return ""
	}

	// First extract the todos array from tool_input
	var input todoWriteToolInput
	if err := json.Unmarshal(toolInput, &input); err != nil {
		return ""
	}

	// Delegate to strategy package for the actual extraction logic
	return strategy.ExtractInProgressTodo(input.Todos)
}

// ExtractLastCompletedTodoFromToolInput extracts the content of the last completed todo item.
// In PostToolUse[TodoWrite], the tool_input contains the NEW todo list where the
// just-finished work is marked as "completed". The last completed item represents
// the work that was just done.
//
// Returns empty string if no completed items exist or JSON is invalid.
func ExtractLastCompletedTodoFromToolInput(toolInput json.RawMessage) string {
	if len(toolInput) == 0 {
		return ""
	}

	// First extract the todos array from tool_input
	var input todoWriteToolInput
	if err := json.Unmarshal(toolInput, &input); err != nil {
		return ""
	}

	// Delegate to strategy package for the actual extraction logic
	return strategy.ExtractLastCompletedTodo(input.Todos)
}

// CountTodosFromToolInput returns the number of todo items in the TodoWrite tool_input.
// Returns 0 if the JSON is invalid or empty.
//
// This function unwraps the outer tool_input object to extract the todos array,
// then delegates to strategy.CountTodos for the actual count.
func CountTodosFromToolInput(toolInput json.RawMessage) int {
	if len(toolInput) == 0 {
		return 0
	}

	// First extract the todos array from tool_input
	var input todoWriteToolInput
	if err := json.Unmarshal(toolInput, &input); err != nil {
		return 0
	}

	// Delegate to strategy package for the actual count
	return strategy.CountTodos(input.Todos)
}
