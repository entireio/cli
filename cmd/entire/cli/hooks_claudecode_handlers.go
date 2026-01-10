// hooks_claudecode_handlers.go contains Claude Code specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// currentSessionIDWithFallback returns the persisted Entire session ID when available.
// Falls back to deriving from the model session ID for backward compatibility.
func currentSessionIDWithFallback(modelSessionID string) string {
	entireSessionID, err := paths.ReadCurrentSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to read current session: %v\n", err)
	}
	if entireSessionID != "" {
		// Validate persisted session ID to fail-fast on corrupted files
		if err := paths.ValidateSessionID(entireSessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: invalid persisted session ID: %v\n", err)
			// Fall through to fallback
		} else if modelSessionID == "" || paths.ModelSessionID(entireSessionID) == modelSessionID {
			return entireSessionID
		} else {
			fmt.Fprintf(os.Stderr, "Warning: persisted session ID does not match hook session ID\n")
		}
	}
	if modelSessionID == "" {
		return ""
	}
	return paths.EntireSessionID(modelSessionID)
}

// hookInputData contains parsed hook input and session identifiers.
type hookInputData struct {
	agent           agent.Agent
	input           *agent.HookInput
	modelSessionID  string
	entireSessionID string
}

// parseAndLogHookInput parses the hook input and sets up logging context.
func parseAndLogHookInput() (*hookInputData, error) {
	// Get the agent for session ID transformation
	ag, err := GetAgent()
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input using agent interface
	input, err := ag.ParseHookInput(agent.HookUserPromptSubmit, os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "user-prompt-submit",
		slog.String("hook", "user-prompt-submit"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	modelSessionID := input.SessionID
	if modelSessionID == "" {
		modelSessionID = unknownSessionID
	}

	// Get the Entire session ID, preferring the persisted value to handle midnight boundary
	entireSessionID := currentSessionIDWithFallback(modelSessionID)

	return &hookInputData{
		agent:           ag,
		input:           input,
		modelSessionID:  modelSessionID,
		entireSessionID: entireSessionID,
	}, nil
}

// checkConcurrentSessions checks for concurrent session conflicts and shows warnings if needed.
// Returns true if the hook should be skipped due to an unresolved conflict.
func checkConcurrentSessions(ag agent.Agent, entireSessionID string) (bool, error) {
	strat := GetStrategy()

	concurrentChecker, ok := strat.(strategy.ConcurrentSessionChecker)
	if !ok {
		return false, nil // Strategy doesn't support concurrent checks
	}

	// Check if this session already acknowledged the warning
	existingState, loadErr := strategy.LoadSessionState(entireSessionID)
	warningAlreadyShown := loadErr == nil && existingState != nil && existingState.ConcurrentWarningShown

	// Check for other active sessions with checkpoints (on current HEAD)
	otherSession, checkErr := concurrentChecker.HasOtherActiveSessionsWithCheckpoints(entireSessionID)
	hasConflict := checkErr == nil && otherSession != nil

	if warningAlreadyShown {
		if hasConflict {
			// Warning was shown and conflict still exists - skip hooks
			return true, nil
		}
		// Warning was shown but conflict is resolved (e.g., user committed)
		// Clear the flag and proceed normally
		if existingState != nil {
			existingState.ConcurrentWarningShown = false
			if saveErr := strategy.SaveSessionState(existingState); saveErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to clear concurrent warning flag: %v\n", saveErr)
			}
		}
		return false, nil
	}

	if hasConflict {
		// First time seeing conflict - show warning
		// Include BaseCommit so session state is complete if conflict later resolves
		repo, err := strategy.OpenRepository()
		if err != nil {
			// Output user-friendly error message via hook response
			if outputErr := outputHookResponse(false, fmt.Sprintf("Failed to open git repository: %v\n\nPlease ensure you're in a git repository and try again.", err)); outputErr != nil {
				return false, outputErr
			}
			return true, nil // Skip hook after outputting response
		}
		head, err := repo.Head()
		if err != nil {
			// Output user-friendly error message via hook response
			if outputErr := outputHookResponse(false, fmt.Sprintf("Failed to get git HEAD: %v\n\nPlease ensure the repository has at least one commit.", err)); outputErr != nil {
				return false, outputErr
			}
			return true, nil // Skip hook after outputting response
		}
		// Derive agent type from agent description (e.g., "Claude Code" from "Claude Code - ...")
		agentType := ag.Description()
		if idx := strings.Index(agentType, " - "); idx > 0 {
			agentType = agentType[:idx]
		}
		newState := &strategy.SessionState{
			SessionID:              entireSessionID,
			BaseCommit:             head.Hash().String(),
			ConcurrentWarningShown: true,
			StartedAt:              time.Now(),
			AgentType:              agentType,
		}
		if saveErr := strategy.SaveSessionState(newState); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save session state: %v\n", saveErr)
		}

		// Get resume command for the other session
		resumeCmd := ag.FormatResumeCommand(ag.ExtractAgentSessionID(otherSession.SessionID))

		// Output blocking JSON response
		if err := outputHookResponse(false, "You have another active session with uncommitted changes. Please commit them first and then start a new Claude session. If you continue here, your prompt and resulting changes will not be captured.\n\nTo resume the active session, close Claude Code and run: "+resumeCmd); err != nil {
			return false, err // Failed to output response
		}
		// Successfully output blocking response - skip rest of hook processing
		return true, nil
	}

	return false, nil
}

// handleSessionInitErrors handles session initialization errors and provides user-friendly messages.
func handleSessionInitErrors(ag agent.Agent, initErr error) error {
	// Check for shadow branch conflict error (worktree conflict)
	var conflictErr *strategy.ShadowBranchConflictError
	if errors.As(initErr, &conflictErr) {
		fmt.Fprintf(os.Stderr, "\n"+
			"Warning: Shadow branch conflict detected!\n\n"+
			"   Branch: %s\n"+
			"   Existing session: %s\n"+
			"   From worktree: %s\n"+
			"   Started: %s\n\n"+
			"   This may indicate another agent session is active from a different worktree,\n"+
			"   or a previous session wasn't completed.\n\n"+
			"   Options:\n"+
			"   1. Commit your changes (git commit) to create a new base commit\n"+
			"   2. Run 'entire rewind reset' to discard the shadow branch and start fresh\n"+
			"   3. Continue the previous session from the original worktree: %s\n\n",
			conflictErr.Branch,
			conflictErr.ExistingSession,
			conflictErr.ExistingWorktree,
			conflictErr.LastActivity.Format(time.RFC822),
			conflictErr.ExistingWorktree,
		)
		return fmt.Errorf("shadow branch conflict: %w", initErr)
	}

	// Check for session ID conflict error (shadow branch has different session)
	var sessionConflictErr *strategy.SessionIDConflictError
	if errors.As(initErr, &sessionConflictErr) {
		// Get agent's resume command format
		resumeCmd := ag.FormatResumeCommand(ag.ExtractAgentSessionID(sessionConflictErr.ExistingSession))
		fmt.Fprintf(os.Stderr, "\n"+
			"Warning: Session ID conflict detected!\n\n"+
			"   Shadow branch: %s\n"+
			"   Existing session: %s\n"+
			"   New session: %s\n\n"+
			"   The shadow branch already has checkpoints from a different session.\n"+
			"   Starting a new session would orphan the existing work.\n\n"+
			"   Options:\n"+
			"   1. Commit your changes (git commit) to create a new base commit\n"+
			"   2. Run 'entire rewind reset' to discard the shadow branch and start fresh\n"+
			"   3. Resume the existing session: %s\n\n",
			sessionConflictErr.ShadowBranch,
			sessionConflictErr.ExistingSession,
			sessionConflictErr.NewSession,
			resumeCmd,
		)
		return fmt.Errorf("session ID conflict: %w", initErr)
	}

	// Unknown error type
	fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", initErr)
	return nil
}

// captureInitialState captures the initial state on user prompt submit.
func captureInitialState() error {
	// Parse hook input and setup logging
	hookData, err := parseAndLogHookInput()
	if err != nil {
		return err
	}

	// UNLESS the situation has changed (e.g., user committed, so no more conflict).
	skipHook, err := checkConcurrentSessions(hookData.agent, hookData.entireSessionID)
	if err != nil {
		return err
	}
	if skipHook {
		return nil
	}

	// CLI captures state directly
	if err := CapturePrePromptState(hookData.entireSessionID); err != nil {
		return err
	}

	// If strategy implements SessionInitializer, call it to initialize session state
	strat := GetStrategy()
	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		// Use agent description, but trim to just the name part (before " - ")
		agentType := hookData.agent.Description()
		if idx := strings.Index(agentType, " - "); idx > 0 {
			agentType = agentType[:idx]
		}
		if initErr := initializer.InitializeSession(hookData.entireSessionID, agentType); initErr != nil {
			if err := handleSessionInitErrors(hookData.agent, initErr); err != nil {
				return err
			}
		}
	}

	return nil
}

// commitWithMetadata commits the session changes with metadata.
func commitWithMetadata() error {
	// Skip on default branch for strategies that don't allow it
	if skip, branchName := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil // Don't fail the hook, just skip
	}

	// Get the agent for hook input parsing and session ID transformation
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input using agent interface
	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "stop",
		slog.String("hook", "stop"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	modelSessionID := input.SessionID
	if modelSessionID == "" {
		modelSessionID = unknownSessionID
	}

	// Get the Entire session ID, preferring the persisted value to handle midnight boundary
	entireSessionID := currentSessionIDWithFallback(modelSessionID)

	// Skip if this session was warned about concurrent sessions and user continued
	if shouldSkipHooksForWarnedSession(entireSessionID) {
		return nil
	}

	transcriptPath := input.SessionRef
	if transcriptPath == "" || !fileExists(transcriptPath) {
		return fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	// Create session metadata folder (SessionMetadataDir transforms model session ID to entire session ID)
	// Use AbsPath to ensure we create at repo root, not relative to cwd
	sessionDir := paths.SessionMetadataDir(modelSessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir // Fallback to relative
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Copy transcript
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := copyFile(transcriptPath, logFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", sessionDir+"/"+paths.TranscriptFileName)

	// Load session state to get transcript offset (for strategies that track it)
	// This is used to only parse NEW transcript lines since the last checkpoint
	var transcriptOffset int
	sessionState, loadErr := strategy.LoadSessionState(entireSessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
	}
	if sessionState != nil {
		transcriptOffset = sessionState.CondensedTranscriptLines
		fmt.Fprintf(os.Stderr, "Session state found: parsing transcript from line %d\n", transcriptOffset)
	}

	// Parse transcript (optionally from offset for strategies that track transcript position)
	// When transcriptOffset > 0, only parse NEW lines since the last checkpoint
	var transcript []transcriptLine
	var totalLines int
	if transcriptOffset > 0 {
		// Parse only NEW lines since last checkpoint
		transcript, totalLines, err = parseTranscriptFromLine(transcriptPath, transcriptOffset)
		if err != nil {
			return fmt.Errorf("failed to parse transcript from line %d: %w", transcriptOffset, err)
		}
		fmt.Fprintf(os.Stderr, "Parsed %d new transcript lines (total: %d)\n", len(transcript), totalLines)
	} else {
		// First prompt or no session state - parse entire transcript
		// Use parseTranscriptFromLine with offset 0 to also get totalLines
		transcript, totalLines, err = parseTranscriptFromLine(transcriptPath, 0)
		if err != nil {
			return fmt.Errorf("failed to parse transcript: %w", err)
		}
	}

	// Extract all prompts since last checkpoint for prompt file
	allPrompts := extractUserPrompts(transcript)
	promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)
	promptContent := strings.Join(allPrompts, "\n\n---\n\n")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted %d prompt(s) to: %s\n", len(allPrompts), sessionDir+"/"+paths.PromptFileName)

	// Extract summary
	summaryFile := filepath.Join(sessionDirAbs, paths.SummaryFileName)
	summary := extractLastAssistantMessage(transcript)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted summary to: %s\n", sessionDir+"/"+paths.SummaryFileName)

	// Get modified files from transcript
	modifiedFiles := extractModifiedFiles(transcript)

	// Generate commit message from last user prompt
	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	commitMessage := generateCommitMessage(lastPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", commitMessage)

	// Get repo root for path conversion (not cwd, since Claude may be in a subdirectory)
	// Using cwd would filter out files in sibling directories (paths starting with ..)
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	// Load pre-prompt state (captured on UserPromptSubmit)
	preState, err := LoadPrePromptState(entireSessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}
	if preState != nil {
		fmt.Fprintf(os.Stderr, "Loaded pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
	}

	// Compute new files (files created during session)
	newFiles, err := ComputeNewFiles(preState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute new files: %v\n", err)
	}

	// Compute deleted files (tracked files that were deleted)
	deletedFiles, err := ComputeDeletedFiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute deleted files: %v\n", err)
	}

	// Filter and normalize all paths (CLI responsibility)
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	relNewFiles := FilterAndNormalizePaths(newFiles, repoRoot)
	relDeletedFiles := FilterAndNormalizePaths(deletedFiles, repoRoot)

	// Check if there are any changes to commit
	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files were modified during this session\n")
		fmt.Fprintf(os.Stderr, "Skipping commit\n")
		// Clean up state even when skipping
		if err := CleanupPrePromptState(entireSessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
		}
		return nil
	}

	logFileChanges(relModifiedFiles, relNewFiles, relDeletedFiles)

	// Create context file before saving changes
	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := createContextFileMinimal(contextFile, commitMessage, entireSessionID, promptFile, summaryFile, transcript); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", sessionDir+"/"+paths.ContextFileName)

	// Get git author from local/global config
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get the configured strategy
	strat := GetStrategy()

	// Ensure strategy setup is in place (auto-installs git hook, gitignore, etc. if needed)
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	// Build fully-populated save context and delegate to strategy
	ctx := strategy.SaveContext{
		SessionID:      entireSessionID,
		ModifiedFiles:  relModifiedFiles,
		NewFiles:       relNewFiles,
		DeletedFiles:   relDeletedFiles,
		MetadataDir:    sessionDir,
		MetadataDirAbs: sessionDirAbs,
		CommitMessage:  commitMessage,
		TranscriptPath: transcriptPath,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
	}

	if err := strat.SaveChanges(ctx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	// Update session state with new transcript position for strategies that create
	// commits on the active branch (auto-commit strategy). This prevents parsing old transcript
	// lines on subsequent checkpoints.
	// Note: Shadow strategy doesn't create commits on the active branch, so its
	// checkpoints don't "consume" the transcript in the same way. Shadow should
	// continue parsing the full transcript to capture all files touched in the session.
	if strat.Name() == strategy.StrategyNameAutoCommit {
		// Create session state lazily if it doesn't exist (backward compat for resumed sessions
		// or if InitializeSession was never called/failed)
		if sessionState == nil {
			sessionState = &strategy.SessionState{
				SessionID: entireSessionID,
			}
		}
		sessionState.CondensedTranscriptLines = totalLines
		sessionState.CheckpointCount++
		if updateErr := strategy.SaveSessionState(sessionState); updateErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update session state: %v\n", updateErr)
		} else {
			fmt.Fprintf(os.Stderr, "Updated session state: transcript position=%d, checkpoint=%d\n",
				totalLines, sessionState.CheckpointCount)
		}
	}

	// Clean up pre-prompt state (CLI responsibility)
	if err := CleanupPrePromptState(entireSessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
	}

	return nil
}

// handlePostTodo handles the PostToolUse[TodoWrite] hook for subagent checkpoints.
// Creates a checkpoint if we're in a subagent context (active pre-task file exists).
// Skips silently if not in subagent context (main agent).
func handlePostTodo() error {
	input, err := parseSubagentCheckpointHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[TodoWrite] input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "post-todo",
		slog.String("hook", "post-todo"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.TranscriptPath),
		slog.String("tool_use_id", input.ToolUseID),
	)

	// Check if this session was warned about concurrent sessions and user continued
	if shouldSkipHooksForWarnedSession(currentSessionIDWithFallback(input.SessionID)) {
		return nil
	}

	// Check if we're in a subagent context by looking for an active pre-task file
	taskToolUseID, found := FindActivePreTaskFile()
	if !found {
		// Not in subagent context - this is a main agent TodoWrite, skip
		return nil
	}

	// Skip on default branch to avoid polluting main/master history
	if skip, branchName := ShouldSkipOnDefaultBranch(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping incremental checkpoint on branch '%s'\n", branchName)
		return nil
	}

	// Detect file changes since last checkpoint
	modified, newFiles, deleted, err := DetectChangedFiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to detect changed files: %v\n", err)
		return nil
	}

	// If no file changes, skip creating a checkpoint
	if len(modified) == 0 && len(newFiles) == 0 && len(deleted) == 0 {
		fmt.Fprintf(os.Stderr, "[entire] No file changes detected, skipping incremental checkpoint\n")
		return nil
	}

	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to get git author: %v\n", err)
		return nil
	}

	// Get the active strategy
	strat := GetStrategy()

	// Ensure strategy setup is complete
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
		return nil
	}

	// Get the session ID from the transcript path or input, then transform to Entire session ID
	sessionID := paths.ExtractSessionIDFromTranscriptPath(input.TranscriptPath)
	if sessionID == "" {
		sessionID = input.SessionID
	}
	entireSessionID := currentSessionIDWithFallback(sessionID)

	// Get next checkpoint sequence
	seq := GetNextCheckpointSequence(entireSessionID, taskToolUseID)

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

	// Build incremental checkpoint context
	ctx := strategy.TaskCheckpointContext{
		SessionID:           entireSessionID,
		ToolUseID:           taskToolUseID,
		ModifiedFiles:       modified,
		NewFiles:            newFiles,
		DeletedFiles:        deleted,
		TranscriptPath:      input.TranscriptPath,
		AuthorName:          author.Name,
		AuthorEmail:         author.Email,
		IsIncremental:       true,
		IncrementalSequence: seq,
		IncrementalType:     input.ToolName,
		IncrementalData:     input.ToolInput,
		TodoContent:         todoContent,
	}

	// Save incremental checkpoint
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save incremental checkpoint: %v\n", err)
		return nil
	}

	fmt.Fprintf(os.Stderr, "[entire] Created incremental checkpoint #%d for %s (task: %s)\n",
		seq, input.ToolName, taskToolUseID[:min(12, len(taskToolUseID))])
	return nil
}

// handlePreTask handles the PreToolUse[Task] hook
func handlePreTask() error {
	// Skip on default branch for strategies that don't allow it
	if skip, branchName := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil // Don't fail the hook, just skip
	}

	input, err := parseTaskHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PreToolUse[Task] input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "pre-task",
		slog.String("hook", "pre-task"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.TranscriptPath),
		slog.String("tool_use_id", input.ToolUseID),
	)

	// Check if this session was warned about concurrent sessions and user continued
	if shouldSkipHooksForWarnedSession(currentSessionIDWithFallback(input.SessionID)) {
		return nil
	}

	// Log context to stdout
	logPreTaskHookContext(os.Stdout, input)

	// Capture pre-task state
	if err := CapturePreTaskState(input.ToolUseID); err != nil {
		return fmt.Errorf("failed to capture pre-task state: %w", err)
	}

	// Create "Starting agent" checkpoint
	// This allows rewinding to the state just before the subagent began
	if err := createStartingAgentCheckpoint(input); err != nil {
		// Log warning but don't fail the hook - state was already captured
		fmt.Fprintf(os.Stderr, "Warning: failed to create starting checkpoint: %v\n", err)
	}

	return nil
}

// createStartingAgentCheckpoint creates a checkpoint commit marking the start of a subagent.
// This is called during PreToolUse[Task] hook.
func createStartingAgentCheckpoint(input *TaskHookInput) error {
	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get the active strategy
	strat := GetStrategy()

	// Ensure strategy setup is complete
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to ensure strategy setup: %w", err)
	}

	entireSessionID := currentSessionIDWithFallback(input.SessionID)

	// Extract subagent type and description from tool_input for descriptive commit messages
	subagentType, taskDescription := ParseSubagentTypeAndDescription(input.ToolInput)

	// Build task checkpoint context for the "starting" checkpoint
	ctx := strategy.TaskCheckpointContext{
		SessionID:       entireSessionID,
		ToolUseID:       input.ToolUseID,
		TranscriptPath:  input.TranscriptPath,
		AuthorName:      author.Name,
		AuthorEmail:     author.Email,
		SubagentType:    subagentType,
		TaskDescription: taskDescription,
		// No file changes yet - this is the starting state
		ModifiedFiles: nil,
		NewFiles:      nil,
		DeletedFiles:  nil,
		// Mark as starting checkpoint (sequence 0)
		IsIncremental:       true,
		IncrementalSequence: 0,
		IncrementalType:     strategy.IncrementalTypeTaskStart,
	}

	// Save the checkpoint
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		return fmt.Errorf("failed to save starting checkpoint: %w", err)
	}

	shortID := input.ToolUseID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	fmt.Fprintf(os.Stderr, "[entire] Created starting checkpoint for task %s\n", shortID)

	return nil
}

// handlePostTask handles the PostToolUse[Task] hook
func handlePostTask() error {
	// Skip on default branch for strategies that don't allow it
	if skip, branchName := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil // Don't fail the hook, just skip
	}

	input, err := parsePostTaskHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[Task] input: %w", err)
	}

	// Extract subagent type from tool_input for logging
	subagentType, taskDescription := ParseSubagentTypeAndDescription(input.ToolInput)

	// Log parsed input context
	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "post-task",
		slog.String("hook", "post-task"),
		slog.String("hook_type", "subagent"),
		slog.String("tool_use_id", input.ToolUseID),
		slog.String("agent_id", input.AgentID),
		slog.String("subagent_type", subagentType),
	)

	// Check if this session was warned about concurrent sessions and user continued
	if shouldSkipHooksForWarnedSession(currentSessionIDWithFallback(input.SessionID)) {
		return nil
	}

	// Determine subagent transcript path
	transcriptDir := filepath.Dir(input.TranscriptPath)
	var subagentTranscriptPath string
	if input.AgentID != "" {
		subagentTranscriptPath = AgentTranscriptPath(transcriptDir, input.AgentID)
		if !fileExists(subagentTranscriptPath) {
			subagentTranscriptPath = ""
		}
	}

	// Log context to stdout
	logPostTaskHookContext(os.Stdout, input, subagentTranscriptPath)

	// Parse transcript to extract modified files
	var modifiedFiles []string
	if subagentTranscriptPath != "" {
		// Use subagent transcript if available
		transcript, err := parseTranscript(subagentTranscriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse subagent transcript: %v\n", err)
		} else {
			modifiedFiles = extractModifiedFiles(transcript)
		}
	} else {
		// Fall back to main transcript
		transcript, err := parseTranscript(input.TranscriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse transcript: %v\n", err)
		} else {
			modifiedFiles = extractModifiedFiles(transcript)
		}
	}

	// Load pre-task state and compute new files
	preState, err := LoadPreTaskState(input.ToolUseID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-task state: %v\n", err)
	}
	newFiles, err := ComputeNewFilesFromTask(preState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute new files: %v\n", err)
	}

	// Get repo root for path conversion (not cwd, since Claude may be in a subdirectory)
	// Using cwd would filter out files in sibling directories (paths starting with ..)
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	// Filter and normalize paths
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	relNewFiles := FilterAndNormalizePaths(newFiles, repoRoot)

	// Find checkpoint UUID from main transcript (best-effort, ignore errors)
	transcript, _ := parseTranscript(input.TranscriptPath) //nolint:errcheck // best-effort extraction
	checkpointUUID, _ := FindCheckpointUUID(transcript, input.ToolUseID)

	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get the configured strategy
	strat := GetStrategy()

	// Ensure strategy setup is in place
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	entireSessionID := currentSessionIDWithFallback(input.SessionID)

	// Build task checkpoint context - strategy handles metadata creation
	// Note: Incremental checkpoints are now created during task execution via handlePostTodo,
	// so we don't need to collect/cleanup staging area here.
	ctx := strategy.TaskCheckpointContext{
		SessionID:              entireSessionID,
		ToolUseID:              input.ToolUseID,
		AgentID:                input.AgentID,
		ModifiedFiles:          relModifiedFiles,
		NewFiles:               relNewFiles,
		DeletedFiles:           nil, // TODO: compute deleted files
		TranscriptPath:         input.TranscriptPath,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         checkpointUUID,
		AuthorName:             author.Name,
		AuthorEmail:            author.Email,
		SubagentType:           subagentType,
		TaskDescription:        taskDescription,
	}

	// Call strategy to save task checkpoint - strategy handles all metadata creation
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		return fmt.Errorf("failed to save task checkpoint: %w", err)
	}

	// Cleanup pre-task state (ignore error - cleanup is best-effort)
	_ = CleanupPreTaskState(input.ToolUseID) //nolint:errcheck // best-effort cleanup

	return nil
}

// handleSessionStart handles the SessionStart hook for Claude Code.
// It reads session info from stdin and sets it as the current session.
func handleSessionStart() error {
	// Get the agent for session ID transformation
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input using agent interface
	input, err := ag.ParseHookInput(agent.HookSessionStart, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "session-start",
		slog.String("hook", "session-start"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	if input.SessionID == "" {
		return errors.New("no session_id in input")
	}

	// Generate the full Entire session ID (with date prefix) from the agent's session ID
	entireSessionID := paths.EntireSessionID(input.SessionID)

	// Write session ID to current_session file
	if err := paths.WriteCurrentSession(entireSessionID); err != nil {
		return fmt.Errorf("failed to set current session: %w", err)
	}

	fmt.Printf("Current session set to: %s\n", entireSessionID)
	return nil
}

// hookResponse represents a JSON response for Claude Code hooks.
// Used to control whether Claude continues processing the prompt.
type hookResponse struct {
	Continue   bool   `json:"continue"`
	StopReason string `json:"stopReason,omitempty"`
}

// outputHookResponse outputs a JSON response to stdout for Claude Code hooks.
// When continueExec is false, Claude will block the current operation and show the reason to the user.
func outputHookResponse(continueExec bool, reason string) error {
	resp := hookResponse{
		Continue:   continueExec,
		StopReason: reason,
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return fmt.Errorf("failed to encode hook response: %w", err)
	}
	return nil
}

// shouldSkipHooksForWarnedSession checks if hooks should be skipped for a session
// that was warned about concurrent sessions and the user continued anyway.
// Returns true only if ConcurrentWarningShown is set AND the conflict still exists.
// If the conflict is resolved (e.g., user committed), clears the flag and returns false.
func shouldSkipHooksForWarnedSession(entireSessionID string) bool {
	state, err := strategy.LoadSessionState(entireSessionID)
	if err != nil || state == nil || !state.ConcurrentWarningShown {
		return false
	}

	// Re-check if the conflict still exists
	strat := GetStrategy()

	concurrentChecker, ok := strat.(strategy.ConcurrentSessionChecker)
	if !ok {
		return false
	}

	otherSession, checkErr := concurrentChecker.HasOtherActiveSessionsWithCheckpoints(entireSessionID)
	if checkErr != nil || otherSession == nil {
		// Conflict is resolved - clear the flag and allow hooks to proceed
		state.ConcurrentWarningShown = false
		// Best effort to save - don't fail if it doesn't work
		if saveErr := strategy.SaveSessionState(state); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to clear concurrent warning flag: %v\n", saveErr)
		}
		return false
	}

	// Conflict still exists - skip hooks
	return true
}
