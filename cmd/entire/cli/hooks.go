package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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

	// AgentID identifies the subagent instance issuing this hook call, per Claude
	// Code's PostToolUse hook docs (top-level "agent_id"). Only present when the hook
	// fires inside a subagent. Used to disambiguate sibling (non-nested) parallel
	// Tasks, whose incremental checkpoints would otherwise be misattributed by
	// FindActivePreTaskFile's "most recently modified" heuristic. Note this is
	// distinct from tool_response.agentId, which only appears on the parent's Task
	// tool result once a subagent finishes (SubagentEnd), not on TodoWrite calls.
	AgentID string `json:"agent_id,omitempty"`
}

// parseSubagentCheckpointHookInput parses PostToolUse hook input for subagent
// checkpoints. It streams a single JSON value rather than reading to EOF so the
// claude-code post-todo hook never blocks waiting for a stdin close that some
// agents don't send on Windows (issue #1398).
func parseSubagentCheckpointHookInput(r io.Reader) (*SubagentCheckpointHookInput, error) {
	return agent.ReadAndParseHookInput[SubagentCheckpointHookInput](r)
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

// backgroundTaskToolInput represents the tool_input structure for the Task
// tool, used to detect a background subagent launch.
type backgroundTaskToolInput struct {
	RunInBackground bool `json:"run_in_background"`
}

// isBackgroundLaunch reports whether a Task tool invocation requested
// run_in_background: true. Mirrors ParseSubagentTypeAndDescription's
// ToolInput parsing. Returns false (foreground) when toolInput is empty or
// invalid — defaulting to the existing foreground behavior is always safe.
func isBackgroundLaunch(ctx context.Context, toolInput json.RawMessage) bool {
	if len(toolInput) == 0 {
		return false
	}

	var input backgroundTaskToolInput
	if err := json.Unmarshal(toolInput, &input); err != nil {
		logging.Debug(ctx, "failed to parse tool_input for background-launch detection; treating as foreground",
			slog.String("error", err.Error()))
		return false
	}

	return input.RunInBackground
}

// todoWriteToolInput represents the tool_input structure for the TodoWrite tool.
// Used to extract the todos array for the strategy-package todo helpers.
type todoWriteToolInput struct {
	Todos json.RawMessage `json:"todos"`
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
