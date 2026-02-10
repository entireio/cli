package strategy

import (
	"encoding/json"
	"fmt"
)

// MaxDescriptionLength is the maximum length for descriptions in commit messages
// before truncation occurs.
const MaxDescriptionLength = 60

// TruncateDescription truncates a string to maxLen characters, adding "..." if truncated.
// If maxLen is less than 3, returns a string of dots up to maxLen.
func TruncateDescription(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// FormatSubagentEndMessage formats a commit message for when a subagent completes.
// Format: "Completed '<agent-type>' agent: <description> (<tool-use-id>)"
//
// Edge cases:
//   - Empty description: "Completed '<agent-type>' agent (<tool-use-id>)"
//   - Empty agentType: "Completed agent: <description> (<tool-use-id>)"
//   - Both empty: "Task: <tool-use-id>"
func FormatSubagentEndMessage(agentType, description, toolUseID string) string {
	return formatSubagentMessage("Completed", agentType, description, toolUseID)
}

// formatSubagentMessage is a shared helper for start/end messages.
func formatSubagentMessage(verb, agentType, description, toolUseID string) string {
	// Both empty - fall back to simple format
	if agentType == "" && description == "" {
		return "Task: " + toolUseID
	}

	// Truncate description if needed
	if description != "" {
		description = TruncateDescription(description, MaxDescriptionLength)
	}

	// Build message based on what fields are present
	if agentType != "" && description != "" {
		return fmt.Sprintf("%s '%s' agent: %s (%s)", verb, agentType, description, toolUseID)
	}
	if agentType != "" {
		return fmt.Sprintf("%s '%s' agent (%s)", verb, agentType, toolUseID)
	}
	// agentType is empty, description is present
	return fmt.Sprintf("%s agent: %s (%s)", verb, description, toolUseID)
}

// FormatIncrementalSubject formats the commit message subject for incremental checkpoints.
// Delegates to FormatIncrementalMessage.
//
// Note: The incrementalType, subagentType, and taskDescription parameters are kept for
// API compatibility but are not currently used. They may be used in the future for
// different checkpoint types.
func FormatIncrementalSubject(
	incrementalType string, //nolint:unparam // kept for API compatibility
	subagentType string, //nolint:unparam // kept for API compatibility
	taskDescription string, //nolint:unparam // kept for API compatibility
	todoContent string,
	incrementalSequence int,
	shortToolUseID string,
) string {
	// Currently all incremental checkpoints use the same format
	_, _, _ = incrementalType, subagentType, taskDescription // Silence unused warnings
	return FormatIncrementalMessage(todoContent, incrementalSequence, shortToolUseID)
}

// FormatIncrementalMessage formats a commit message for an incremental checkpoint.
// Format: "<todo-content> (<tool-use-id>)"
//
// If todoContent is empty, falls back to: "Checkpoint #<sequence>: <tool-use-id>"
func FormatIncrementalMessage(todoContent string, sequence int, toolUseID string) string {
	if todoContent == "" {
		return fmt.Sprintf("Checkpoint #%d: %s", sequence, toolUseID)
	}

	// Truncate todo content if needed
	todoContent = TruncateDescription(todoContent, MaxDescriptionLength)
	return fmt.Sprintf("%s (%s)", todoContent, toolUseID)
}

// todoItem represents a single item in the TodoWrite tool_input.todos array.
type todoItem struct {
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Status     string `json:"status"`
}

// ExtractLastCompletedTodo extracts the content of the last completed todo item from tool_input.
// This represents the work that was just finished and is used for commit messages.
//
// When TodoWrite is called in PostToolUse, the NEW list is provided which has the
// just-completed work marked as "completed". The last completed item is the most
// recently finished task.
//
// Returns empty string if no completed items exist or JSON is invalid.
func ExtractLastCompletedTodo(todosJSON []byte) string {
	if len(todosJSON) == 0 {
		return ""
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return ""
	}

	// Find the last completed item - this is the work that was just finished
	var lastCompleted string
	for _, todo := range todos {
		if todo.Status == "completed" {
			lastCompleted = todo.Content
		}
	}
	return lastCompleted
}

// CountTodos returns the number of todo items in the JSON array.
// Returns 0 if the JSON is invalid or empty.
func CountTodos(todosJSON []byte) int {
	if len(todosJSON) == 0 {
		return 0
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return 0
	}

	return len(todos)
}

// ExtractInProgressTodo extracts the content of the in-progress todo item from tool_input.
// This is used for commit messages in incremental checkpoints.
//
// Priority order:
//  1. in_progress item (current work)
//  2. first pending item (next work - fallback)
//  3. last completed item (final work just finished)
//  4. first item with unknown status (edge case)
//  5. empty string (no items)
//
// Returns empty string if no suitable item is found or JSON is invalid.
func ExtractInProgressTodo(todosJSON []byte) string {
	if len(todosJSON) == 0 {
		return ""
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return ""
	}

	if len(todos) == 0 {
		return ""
	}

	// Look for in_progress item first (case-sensitive match)
	for _, todo := range todos {
		if todo.Status == "in_progress" {
			return todo.Content
		}
	}

	// Fall back to first pending item
	for _, todo := range todos {
		if todo.Status == "pending" {
			return todo.Content
		}
	}

	// Fall back to last completed item (represents the work that was just finished)
	var lastCompleted string
	for _, todo := range todos {
		if todo.Status == "completed" {
			lastCompleted = todo.Content
		}
	}
	if lastCompleted != "" {
		return lastCompleted
	}

	// If no in_progress, pending, or completed items, but there are items with
	// unrecognized status, return first item's content as a fallback (handles edge cases).
	if todos[0].Content != "" {
		return todos[0].Content
	}

	return ""
}
