package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TaskHookInput represents the JSON input from PreToolUse[Task] hook
type TaskHookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// postTaskHookInputRaw is the raw JSON structure from PostToolUse[Task] hook
type postTaskHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   struct {
		AgentID string `json:"agentId"`
	} `json:"tool_response"`
}

// PostTaskHookInput represents the parsed input from PostToolUse[Task] hook
type PostTaskHookInput struct {
	TaskHookInput

	AgentID   string          // Extracted from tool_response.agentId
	ToolInput json.RawMessage // Raw tool input for reference
}

// parseTaskHookInput parses PreToolUse[Task] hook input from reader
func parseTaskHookInput(r io.Reader) (*TaskHookInput, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var input TaskHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &input, nil
}

// parsePostTaskHookInput parses PostToolUse[Task] hook input from reader
func parsePostTaskHookInput(r io.Reader) (*PostTaskHookInput, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var raw postTaskHookInputRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &PostTaskHookInput{
		TaskHookInput: TaskHookInput{
			SessionID:      raw.SessionID,
			TranscriptPath: raw.TranscriptPath,
			ToolUseID:      raw.ToolUseID,
		},
		AgentID:   raw.ToolResponse.AgentID,
		ToolInput: raw.ToolInput,
	}, nil
}

// logPreTaskHookContext logs the PreToolUse[Task] hook context to the writer
func logPreTaskHookContext(w io.Writer, input *TaskHookInput) {
	_, _ = fmt.Fprintln(w, "[entire] PreToolUse[Task] hook invoked")
	_, _ = fmt.Fprintf(w, "  Session ID: %s\n", input.SessionID)
	_, _ = fmt.Fprintf(w, "  Tool Use ID: %s\n", input.ToolUseID)
	_, _ = fmt.Fprintf(w, "  Transcript: %s\n", input.TranscriptPath)
}

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

// logPostTaskHookContext logs the PostToolUse[Task] hook context to the writer
func logPostTaskHookContext(w io.Writer, input *PostTaskHookInput, subagentTranscriptPath string) {
	_, _ = fmt.Fprintln(w, "[entire] PostToolUse[Task] hook invoked")
	_, _ = fmt.Fprintf(w, "  Session ID: %s\n", input.SessionID)
	_, _ = fmt.Fprintf(w, "  Tool Use ID: %s\n", input.ToolUseID)

	if input.AgentID != "" {
		_, _ = fmt.Fprintf(w, "  Agent ID: %s\n", input.AgentID)
	} else {
		_, _ = fmt.Fprintln(w, "  Agent ID: (none)")
	}

	_, _ = fmt.Fprintf(w, "  Transcript: %s\n", input.TranscriptPath)

	if subagentTranscriptPath != "" {
		_, _ = fmt.Fprintf(w, "  Subagent Transcript: %s\n", subagentTranscriptPath)
	} else {
		_, _ = fmt.Fprintln(w, "  Subagent Transcript: (none)")
	}
}

// handleSessionStartCommon is the shared implementation for session start hooks.
// Used by both Claude Code and Gemini CLI handlers.
func handleSessionStartCommon() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookSessionStart, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "session-start",
		slog.String("hook", "session-start"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	if input.SessionID == "" {
		return errors.New("no session_id in input")
	}

	// Build informational message
	message := "\n\nPowered by Entire:\n  This conversation will be linked to your next commit."

	// Append wingman note if enabled
	if settings.IsWingmanEnabled() {
		message += "\n  Wingman is active: your changes will be automatically reviewed."
	}

	// Check for concurrent sessions and append count if any
	strat := GetStrategy()
	if concurrentChecker, ok := strat.(strategy.ConcurrentSessionChecker); ok {
		if count, err := concurrentChecker.CountOtherActiveSessionsWithCheckpoints(input.SessionID); err == nil && count > 0 {
			message += fmt.Sprintf("\n  %d other active conversation(s) in this workspace will also be included.\n  Use 'entire status' for more information.", count)
		}
	}

	// Output informational message using agent-specific format
	if err := outputHookResponse(message); err != nil {
		return err
	}

	// Fire EventSessionStart for the current session (if state exists).
	// This handles ENDED → IDLE (re-entering a session).
	// TODO(ENT-221): dispatch ActionWarnStaleSession for ACTIVE/ACTIVE_COMMITTED sessions.
	if state, loadErr := strategy.LoadSessionState(input.SessionID); loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state on start: %v\n", loadErr)
	} else if state != nil {
		strategy.TransitionAndLog(state, session.EventSessionStart, session.TransitionContext{})
		if saveErr := strategy.SaveSessionState(state); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update session state on start: %v\n", saveErr)
		}
	}

	return nil
}

// hookSpecificOutput contains event-specific fields nested under hookSpecificOutput
// in the hook response JSON. Claude Code requires this nesting for additionalContext
// to be injected into the agent's conversation.
type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// hookResponse represents a JSON response to a Claude Code hook.
// systemMessage is shown to the user as a warning/info message.
// hookSpecificOutput contains event-specific fields like additionalContext.
type hookResponse struct {
	SystemMessage      string              `json:"systemMessage,omitempty"`
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// outputHookResponse outputs a JSON response with additionalContext for
// SessionStart hooks. The context is injected into the agent's conversation.
func outputHookResponse(context string) error {
	resp := hookResponse{
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: context,
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return fmt.Errorf("failed to encode hook response: %w", err)
	}
	return nil
}

// outputHookResponseWithContextAndMessage outputs a JSON response with both
// additionalContext (injected into agent conversation) and a systemMessage
// (shown to the user as a warning/info).
func outputHookResponseWithContextAndMessage(context, message string) error {
	resp := hookResponse{
		SystemMessage: message,
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "UserPromptSubmit",
			AdditionalContext: context,
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return fmt.Errorf("failed to encode hook response: %w", err)
	}
	return nil
}
