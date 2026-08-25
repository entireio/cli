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
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
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
		slog.String("agent_id", input.AgentID),
	)

	// Resolve which task this incremental checkpoint belongs to. Not in subagent
	// context (no task resolved at all) means this is a main agent TodoWrite, skip.
	taskToolUseID, found := resolveIncrementalCheckpointTask(logCtx, input.AgentID)
	if !found {
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
		logStatusDegrade(logCtx, "failed to detect changed files", err)
		return nil
	}

	// Same guard as turn-end/subagent-end: when the pre-task untracked scan
	// was skipped (e.g. status-walk budget breach at task start), there is no
	// baseline, and the nil baseline above classifies EVERY untracked file as
	// New — so pre-existing untracked files would be claimed by this
	// incremental checkpoint.
	if preState, preErr := LoadPreTaskState(ctx, taskToolUseID); preErr != nil {
		logging.Warn(logCtx, "failed to load pre-task state",
			slog.String("error", preErr.Error()))
	} else if preState != nil && preState.UntrackedScanSkipped {
		logging.Warn(logCtx, "skipping new-file detection: pre-task untracked scan was skipped")
		changes.New = nil
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

// resolveIncrementalCheckpointTask determines which task's incremental checkpoint an
// PostToolUse[TodoWrite] hook invocation belongs to. Returns ("", false) if there is no
// active task at all (main agent context).
//
// Claude Code runs sibling (non-nested) parallel Tasks concurrently, each with its own
// pre-task file. FindActivePreTaskFile's "most recently modified" heuristic breaks down
// once a sibling's pre-task file becomes the newest after this agent's Task already
// started: its TodoWrite progress would get misattributed to the wrong task.
//
// When agentID identifies the calling subagent instance (top-level agent_id on
// PostToolUse hook input), a previously remembered agent->task link is preferred over
// the mtime heuristic. The first time we resolve a task for a given agentID, the link is
// created so every subsequent TodoWrite from that same subagent instance sticks to its
// own task regardless of what other siblings do.
//
// Bootstrap prefers an unclaimed pre-task (no existing agent-task link) so two siblings
// whose first PostTodos race each other do not both latch onto the same mtime winner.
// Each PostTodo hook invocation is a separate OS process, so the unclaimed-lookup and
// the link-write below are additionally serialized with agentTaskBootstrapLockPath: two
// siblings' first PostTodos firing at the same instant could otherwise both read the same
// unclaimed pre-task before either had written its claim, and double-claim it.
func resolveIncrementalCheckpointTask(ctx context.Context, agentID string) (taskToolUseID string, found bool) {
	if agentID != "" {
		if linked, ok := LookupAgentTaskLink(ctx, agentID); ok {
			return linked, true
		}
	}

	if agentID != "" {
		release, err := flock.Acquire(agentTaskBootstrapLockPath(ctx))
		if err != nil {
			// The lock is required, not best-effort: proceeding without it would
			// recreate the exact double-claim race this function exists to close.
			// Bailing loses at most one incremental checkpoint for this call; a
			// later TodoWrite from the same agentID retries the whole bootstrap.
			logging.Warn(ctx, "failed to acquire agent-task bootstrap lock; skipping bootstrap for this call",
				slog.String("error", err.Error()))
			return "", false
		}
		defer release()
		// Re-check under the lock: another sibling's bootstrap may have remembered
		// our link while we were waiting to acquire it.
		if linked, ok := LookupAgentTaskLink(ctx, agentID); ok {
			return linked, true
		}
	}

	if agentID != "" {
		var candidates int
		taskToolUseID, candidates, found = FindUnclaimedActivePreTaskFile(ctx)
		if found && candidates > 1 {
			// Spawn order is the only signal available here, so with several
			// unclaimed siblings this pairing may not be the true one. Log it so
			// a misattributed checkpoint is diagnosable rather than silent.
			logging.Warn(ctx, "bootstrapping agent-task link with several unclaimed pre-tasks; assignment follows spawn order and may not be this agent's own task",
				slog.String("agent_id", agentID),
				slog.Int("unclaimed_candidates", candidates),
				slog.String("task", taskToolUseID[:min(12, len(taskToolUseID))]))
		}
	}
	if !found {
		taskToolUseID, found = FindActivePreTaskFile(ctx)
		if found && agentID != "" {
			logging.Warn(ctx, "bootstrapping agent-task link from mtime heuristic; no unclaimed pre-task left",
				slog.String("agent_id", agentID),
				slog.String("task", taskToolUseID[:min(12, len(taskToolUseID))]))
		}
	}
	if !found {
		return "", false
	}

	if agentID != "" {
		if err := RememberAgentTaskLink(ctx, agentID, taskToolUseID); err != nil {
			// Fail closed, matching the lock-acquisition policy above. Returning
			// the task anyway would checkpoint against a claim that was never
			// durably recorded, so a later sibling could select the same task and
			// this agent could be remapped on its next TodoWrite. Losing this one
			// incremental checkpoint is recoverable; the next TodoWrite retries
			// the whole bootstrap.
			logging.Warn(ctx, "failed to remember agent-task link; skipping this incremental checkpoint",
				slog.String("agent_id", agentID),
				slog.String("error", err.Error()))
			return "", false
		}
	}
	return taskToolUseID, true
}

// agentTaskBootstrapLockPath returns the path to the advisory lock guarding the
// unclaimed-pre-task lookup + agent-task link write in resolveIncrementalCheckpointTask.
// A single fixed path (not per-agent) is intentional: the race being closed is between
// DIFFERENT agentIDs racing to claim the same unclaimed pre-task, so the critical section
// must be exclusive across all of them.
func agentTaskBootstrapLockPath(ctx context.Context) string {
	return filepath.Join(resolveTmpDir(ctx), "agent-task-bootstrap.lock")
}
