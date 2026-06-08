// hooks_claudecode_posttodo.go contains the PostTodo hook handler for Claude Code.
// This is a Claude-specific hook that creates incremental checkpoints during subagent execution.
// It's not part of the generic lifecycle dispatcher because it requires special handling:
// - Only fires for TodoWrite tool invocations
// - Creates incremental checkpoints (not full checkpoints)
// - Only activates when in subagent context (pre-task file exists)
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// handleClaudeCodePostTodo handles the PostToolUse[TodoWrite] hook for subagent checkpoints.
// Creates a checkpoint if we're in a subagent context (active pre-task file exists).
// Skips silently if not in subagent context (main agent).
func handleClaudeCodePostTodo(ctx context.Context) error {
	return handleClaudeCodePostTodoFromReader(ctx, os.Stdin)
}

// handleClaudeCodePostTodoFromReader is the testable version that accepts an io.Reader.
func handleClaudeCodePostTodoFromReader(ctx context.Context, reader io.Reader) error {
	input, err := parseSubagentCheckpointHookInput(reader)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[TodoWrite] input: %w", err)
	}

	// PostTodo bypasses DispatchLifecycleEvent (see hook_registry.go), so the
	// central guard in lifecycle.go never runs here. Mirror it: SessionID and
	// ToolUseID get used to build filesystem paths downstream (session-state
	// lookups, pre-task file naming, TaskStepContext), and TranscriptPath is
	// consumed as an os.ReadFile/filepath.Dir target.
	if input.SessionID != "" {
		if err := validation.ValidateSessionID(input.SessionID); err != nil {
			return fmt.Errorf("invalid session ID in PostTodo input: %w", err)
		}
	}
	if err := validation.ValidateToolUseID(input.ToolUseID); err != nil {
		return fmt.Errorf("invalid tool use ID in PostTodo input: %w", err)
	}
	if err := validation.ValidateSessionRef(input.TranscriptPath); err != nil {
		return fmt.Errorf("invalid transcript path in PostTodo input: %w", err)
	}

	// Get agent for logging context
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(ctx, "hooks"), ag.Name())
	logging.Info(logCtx, "post-todo",
		slog.String("hook", "post-todo"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.TranscriptPath),
		slog.String("tool_use_id", input.ToolUseID),
	)

	// Check if we're in a subagent context by looking for an active pre-task file
	taskToolUseID, found := FindActivePreTaskFile(ctx)
	if !found {
		// Not in subagent context - this is a main agent TodoWrite, skip
		return nil
	}

	// Skip on default branch to avoid polluting main/master history
	if skip, branchName := ShouldSkipOnDefaultBranch(ctx); skip {
		logging.Info(logCtx, "skipping incremental checkpoint on default branch",
			slog.String("branch", branchName))
		return nil
	}

	// Detect file changes since last checkpoint
	changes, err := DetectFileChanges(ctx, nil)
	if err != nil {
		logging.Warn(logCtx, "failed to detect changed files",
			slog.String("error", err.Error()))
		return nil
	}

	// If no file changes, skip creating a checkpoint
	if len(changes.Modified) == 0 && len(changes.New) == 0 && len(changes.Deleted) == 0 {
		logging.Info(logCtx, "no file changes detected, skipping incremental checkpoint")
		return nil
	}

	// Get git author
	author, err := GetGitAuthor(ctx)
	if err != nil {
		logging.Warn(logCtx, "failed to get git author",
			slog.String("error", err.Error()))
		return nil
	}

	// Get the active strategy
	strat := GetStrategy(ctx)

	// Get the session ID from the transcript path or input, then transform to Entire session ID
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = paths.ExtractSessionIDFromTranscriptPath(input.TranscriptPath)
	}

	// Get next checkpoint sequence
	seq := GetNextCheckpointSequence(sessionID, taskToolUseID)

	// Extract the todo content from the tool_input.
	// PostToolUse receives the NEW todo list where the just-completed work is
	// marked as "completed". The last completed item is the work that was just done.
	todoContent := ExtractLastCompletedTodoFromToolInput(input.ToolInput)
	if todoContent == "" {
		// No completed items - this is likely the first TodoWrite (planning phase).
		// Check if there are any todos at all to avoid duplicate messages.
		todoCount := CountTodosFromToolInput(input.ToolInput)
		if todoCount > 0 {
			// Use "Planning: N todos" format for the first TodoWrite
			todoContent = fmt.Sprintf("Planning: %d todos", todoCount)
		}
		// If todoCount == 0, todoContent remains empty and FormatIncrementalMessage
		// will fall back to "Checkpoint #N" format
	}

	// Get agent type from the currently executing hook agent (authoritative source)
	var agentType types.AgentType
	if hookAgent, agentErr := GetCurrentHookAgent(); agentErr == nil {
		agentType = hookAgent.Type()
	}

	// Build incremental task step context
	taskStepCtx := strategy.TaskStepContext{
		SessionID:           sessionID,
		ToolUseID:           taskToolUseID,
		ModifiedFiles:       changes.Modified,
		NewFiles:            changes.New,
		DeletedFiles:        changes.Deleted,
		TranscriptPath:      input.TranscriptPath,
		AuthorName:          author.Name,
		AuthorEmail:         author.Email,
		IsIncremental:       true,
		IncrementalSequence: seq,
		IncrementalType:     input.ToolName,
		IncrementalData:     input.ToolInput,
		TodoContent:         todoContent,
		AgentType:           agentType,
	}

	// Save incremental task step
	if err := strat.SaveTaskStep(ctx, taskStepCtx); err != nil {
		logging.Warn(logCtx, "failed to save incremental task step",
			slog.String("error", err.Error()))
		return nil
	}

	logging.Info(logCtx, "created incremental checkpoint",
		slog.Int("sequence", seq),
		slog.String("tool_name", input.ToolName),
		slog.String("task", taskToolUseID[:min(12, len(taskToolUseID))]))
	return nil
}
