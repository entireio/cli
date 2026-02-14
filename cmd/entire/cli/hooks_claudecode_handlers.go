// hooks_claudecode_handlers.go contains Claude Code specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// hookInputData contains parsed hook input and session identifiers.
type hookInputData struct {
	agent     agent.Agent
	input     *agent.HookInput
	sessionID string
}

// parseAndLogHookInput parses the hook input and sets up logging context.
func parseAndLogHookInput() (*hookInputData, error) {
	// Get the agent from the hook command context (e.g., "entire hooks claude-code ...")
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input using agent interface
	input, err := ag.ParseHookInput(agent.HookUserPromptSubmit, os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "user-prompt-submit",
		slog.String("hook", "user-prompt-submit"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Get the Entire session ID, preferring the persisted value to handle midnight boundary
	return &hookInputData{
		agent:     ag,
		input:     input,
		sessionID: sessionID,
	}, nil
}

// captureInitialState captures the initial state on user prompt submit.
func captureInitialState() error {
	// Parse hook input and setup logging
	hookData, err := parseAndLogHookInput()
	if err != nil {
		return err
	}

	// CLI captures state directly (including transcript position)
	if err := CapturePrePromptState(hookData.sessionID, hookData.input.SessionRef); err != nil {
		return err
	}

	// If strategy implements SessionInitializer, call it to initialize session state
	strat := GetStrategy()

	// Ensure strategy setup is in place (git hooks, gitignore, metadata branch).
	// Done here at turn start so hooks are installed before any mid-turn commits.
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		agentType := hookData.agent.Type()
		if err := initializer.InitializeSession(hookData.sessionID, agentType, hookData.input.SessionRef, hookData.input.UserPrompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
		}
	}

	return nil
}

// commitWithMetadata commits the session changes with metadata.
func commitWithMetadata() error { //nolint:maintidx // already present in codebase
	// Get the agent for hook input parsing and session ID transformation
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input using agent interface
	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "stop",
		slog.String("hook", "stop"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	transcriptPath := input.SessionRef
	if transcriptPath == "" || !fileExists(transcriptPath) {
		return fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	// Early check: bail out quickly if the repo has no commits yet.
	// Without this, the function does a lot of work (transcript copy, prompt extraction, etc.)
	// before eventually failing deep in strategy.SaveChanges().
	if repo, err := strategy.OpenRepository(); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(os.Stderr, "Entire: skipping checkpoint. Will activate after first commit.")
		return NewSilentError(strategy.ErrEmptyRepository)
	}

	// Create session metadata folder using the entire session ID (preserves original date on resume)
	// Use AbsPath to ensure we create at repo root, not relative to cwd
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir // Fallback to relative
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Wait for Claude Code to flush the transcript file.
	// The stop hook fires before the transcript is fully written to disk.
	// We poll for our own hook_progress sentinel entry in the file tail,
	// which guarantees all prior entries have been flushed.
	waitForTranscriptFlush(transcriptPath, time.Now())

	// Copy transcript
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := copyFile(transcriptPath, logFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", sessionDir+"/"+paths.TranscriptFileName)

	// Load pre-prompt state (captured on UserPromptSubmit)
	// Needed for transcript offset and file change detection
	preState, err := LoadPrePromptState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}

	// Determine transcript offset: prefer pre-prompt state, fall back to session state.
	// Pre-prompt state has the offset when the transcript path was available at prompt time.
	// Session state has the offset updated after each successful checkpoint save (auto-commit).
	var transcriptOffset int
	if preState != nil && preState.StepTranscriptStart > 0 {
		transcriptOffset = preState.StepTranscriptStart
		fmt.Fprintf(os.Stderr, "Pre-prompt state found: parsing transcript from line %d\n", transcriptOffset)
	} else {
		// Fall back to session state (e.g., auto-commit strategy updates it after each save)
		sessionState, loadErr := strategy.LoadSessionState(sessionID)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		}
		if sessionState != nil && sessionState.CheckpointTranscriptStart > 0 {
			transcriptOffset = sessionState.CheckpointTranscriptStart
			fmt.Fprintf(os.Stderr, "Session state found: parsing transcript from line %d\n", transcriptOffset)
		}
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

	if preState != nil {
		fmt.Fprintf(os.Stderr, "Pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
	}

	// Compute new and deleted files (single git status call)
	changes, err := DetectFileChanges(preState.PreUntrackedFiles())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// Filter and normalize all paths (CLI responsibility)
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	// Check if there are any changes to commit
	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files were modified during this session\n")
		fmt.Fprintf(os.Stderr, "Skipping commit\n")
		// Still transition phase even when skipping commit — the turn is ending.
		transitionSessionTurnEnd(sessionID)
		// Clean up state even when skipping
		if err := CleanupPrePromptState(sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "Files modified during session (%d):\n", len(relModifiedFiles))
	for _, file := range relModifiedFiles {
		fmt.Fprintf(os.Stderr, "  - %s\n", file)
	}
	if len(relNewFiles) > 0 {
		fmt.Fprintf(os.Stderr, "New files created (%d):\n", len(relNewFiles))
		for _, file := range relNewFiles {
			fmt.Fprintf(os.Stderr, "  + %s\n", file)
		}
	}
	if len(relDeletedFiles) > 0 {
		fmt.Fprintf(os.Stderr, "Files deleted (%d):\n", len(relDeletedFiles))
		for _, file := range relDeletedFiles {
			fmt.Fprintf(os.Stderr, "  - %s\n", file)
		}
	}

	// Create context file before saving changes
	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := createContextFileMinimal(contextFile, commitMessage, sessionID, promptFile, summaryFile, transcript); err != nil {
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

	// Get agent type from the currently executing hook agent (authoritative source)
	var agentType agent.AgentType
	if hookAgent, agentErr := GetCurrentHookAgent(); agentErr == nil {
		agentType = hookAgent.Type()
	}

	// Get transcript position from pre-prompt state (captured at step/turn start)
	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.StepTranscriptStart
	}

	// Calculate token usage for this checkpoint (Claude Code specific)
	var tokenUsage *agent.TokenUsage
	if transcriptPath != "" {
		// Subagents are stored in a subagents/ directory next to the main transcript
		subagentsDir := filepath.Join(filepath.Dir(transcriptPath), sessionID, "subagents")
		usage, err := claudecode.CalculateTotalTokenUsage(transcriptPath, transcriptLinesAtStart, subagentsDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to calculate token usage: %v\n", err)
		} else {
			tokenUsage = usage
		}
	}

	// Build fully-populated save context and delegate to strategy
	ctx := strategy.SaveContext{
		SessionID:                sessionID,
		ModifiedFiles:            relModifiedFiles,
		NewFiles:                 relNewFiles,
		DeletedFiles:             relDeletedFiles,
		MetadataDir:              sessionDir,
		MetadataDirAbs:           sessionDirAbs,
		CommitMessage:            commitMessage,
		TranscriptPath:           transcriptPath,
		AuthorName:               author.Name,
		AuthorEmail:              author.Email,
		AgentType:                agentType,
		StepTranscriptIdentifier: transcriptIdentifierAtStart,
		StepTranscriptStart:      transcriptLinesAtStart,
		TokenUsage:               tokenUsage,
	}

	if err := strat.SaveChanges(ctx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	// Update session state with new transcript position for strategies that create
	// commits on the active branch (auto-commit strategy). This prevents parsing old transcript
	// lines on subsequent checkpoints.
	// Note: Shadow strategy tracks transcript position per-step via StepTranscriptStart in
	// pre-prompt state, but doesn't advance CheckpointTranscriptStart in session state because
	// its checkpoints accumulate all files touched across the entire session.
	if strat.Name() == strategy.StrategyNameAutoCommit {
		// Load session state for updating transcript position
		sessionState, loadErr := strategy.LoadSessionState(sessionID)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		}
		// Create session state lazily if it doesn't exist (backward compat for resumed sessions
		// or if InitializeSession was never called/failed)
		if sessionState == nil {
			sessionState = &strategy.SessionState{
				SessionID: sessionID,
			}
		}
		sessionState.CheckpointTranscriptStart = totalLines
		sessionState.StepCount++
		if updateErr := strategy.SaveSessionState(sessionState); updateErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update session state: %v\n", updateErr)
		} else {
			fmt.Fprintf(os.Stderr, "Updated session state: transcript position=%d, checkpoint=%d\n",
				totalLines, sessionState.StepCount)
		}
	}

	// Fire EventTurnEnd to transition session phase (all strategies).
	// This moves ACTIVE → IDLE or ACTIVE_COMMITTED → IDLE.
	// For ACTIVE_COMMITTED → IDLE, HandleTurnEnd dispatches ActionCondense.
	transitionSessionTurnEnd(sessionID)

	// Clean up pre-prompt state (CLI responsibility)
	if err := CleanupPrePromptState(sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
	}

	return nil
}

// handleClaudeCodePostTodo handles the PostToolUse[TodoWrite] hook for subagent checkpoints.
// Creates a checkpoint if we're in a subagent context (active pre-task file exists).
// Skips silently if not in subagent context (main agent).
func handleClaudeCodePostTodo() error {
	input, err := parseSubagentCheckpointHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[TodoWrite] input: %w", err)
	}

	// Get agent for logging context
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "post-todo",
		slog.String("hook", "post-todo"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.TranscriptPath),
		slog.String("tool_use_id", input.ToolUseID),
	)

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
	changes, err := DetectFileChanges(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to detect changed files: %v\n", err)
		return nil
	}

	// If no file changes, skip creating a checkpoint
	if len(changes.Modified) == 0 && len(changes.New) == 0 && len(changes.Deleted) == 0 {
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
	var agentType agent.AgentType
	if hookAgent, agentErr := GetCurrentHookAgent(); agentErr == nil {
		agentType = hookAgent.Type()
	}

	// Build incremental checkpoint context
	ctx := strategy.TaskCheckpointContext{
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

	// Save incremental checkpoint
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save incremental checkpoint: %v\n", err)
		return nil
	}

	fmt.Fprintf(os.Stderr, "[entire] Created incremental checkpoint #%d for %s (task: %s)\n",
		seq, input.ToolName, taskToolUseID[:min(12, len(taskToolUseID))])
	return nil
}

// handleClaudeCodePreTask handles the PreToolUse[Task] hook
func handleClaudeCodePreTask() error {
	input, err := parseTaskHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PreToolUse[Task] input: %w", err)
	}

	// Get agent for logging context
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "pre-task",
		slog.String("hook", "pre-task"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.TranscriptPath),
		slog.String("tool_use_id", input.ToolUseID),
	)

	// Log context to stdout
	logPreTaskHookContext(os.Stdout, input)

	// Capture pre-task state locally (for computing new files when task completes).
	// We don't create a shadow branch commit here. Commits are created during
	// task completion (handleClaudeCodePostTask/handleClaudeCodePostTodo) only if the task resulted
	// in file changes.
	if err := CapturePreTaskState(input.ToolUseID); err != nil {
		return fmt.Errorf("failed to capture pre-task state: %w", err)
	}

	return nil
}

// handleClaudeCodePostTask handles the PostToolUse[Task] hook
func handleClaudeCodePostTask() error {
	input, err := parsePostTaskHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[Task] input: %w", err)
	}

	// Extract subagent type from tool_input for logging
	subagentType, taskDescription := ParseSubagentTypeAndDescription(input.ToolInput)

	// Get agent for logging context
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Log parsed input context
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "post-task",
		slog.String("hook", "post-task"),
		slog.String("hook_type", "subagent"),
		slog.String("tool_use_id", input.ToolUseID),
		slog.String("agent_id", input.AgentID),
		slog.String("subagent_type", subagentType),
	)

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

	// Compute new and deleted files (single git status call)
	changes, err := DetectFileChanges(preState.PreUntrackedFiles())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// Get repo root for path conversion (not cwd, since Claude may be in a subdirectory)
	// Using cwd would filter out files in sibling directories (paths starting with ..)
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	// Filter and normalize paths
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	// If no file changes, skip creating a checkpoint
	if len(relModifiedFiles) == 0 && len(relNewFiles) == 0 && len(relDeletedFiles) == 0 {
		fmt.Fprintf(os.Stderr, "[entire] No file changes detected, skipping task checkpoint\n")
		// Cleanup pre-task state (ignore error - cleanup is best-effort)
		_ = CleanupPreTaskState(input.ToolUseID) //nolint:errcheck // best-effort cleanup
		return nil
	}

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

	// Get agent type from the currently executing hook agent (authoritative source)
	var agentType agent.AgentType
	if hookAgent, agentErr := GetCurrentHookAgent(); agentErr == nil {
		agentType = hookAgent.Type()
	}

	// Build task checkpoint context - strategy handles metadata creation
	// Note: Incremental checkpoints are now created during task execution via handleClaudeCodePostTodo,
	// so we don't need to collect/cleanup staging area here.
	ctx := strategy.TaskCheckpointContext{
		SessionID:              input.SessionID,
		ToolUseID:              input.ToolUseID,
		AgentID:                input.AgentID,
		ModifiedFiles:          relModifiedFiles,
		NewFiles:               relNewFiles,
		DeletedFiles:           relDeletedFiles,
		TranscriptPath:         input.TranscriptPath,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         checkpointUUID,
		AuthorName:             author.Name,
		AuthorEmail:            author.Email,
		SubagentType:           subagentType,
		TaskDescription:        taskDescription,
		AgentType:              agentType,
	}

	// Call strategy to save task checkpoint - strategy handles all metadata creation
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		return fmt.Errorf("failed to save task checkpoint: %w", err)
	}

	// Cleanup pre-task state (ignore error - cleanup is best-effort)
	_ = CleanupPreTaskState(input.ToolUseID) //nolint:errcheck // best-effort cleanup

	return nil
}

// handleClaudeCodeSessionStart handles the SessionStart hook for Claude Code.
func handleClaudeCodeSessionStart() error {
	return handleSessionStartCommon()
}

// handleClaudeCodeSessionEnd handles the SessionEnd hook for Claude Code.
// This fires when the user explicitly closes the session.
// Updates the session state with EndedAt timestamp.
func handleClaudeCodeSessionEnd() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookSessionEnd, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "session-end",
		slog.String("hook", "session-end"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
	)

	if input.SessionID == "" {
		return nil // No session to update
	}

	// Best-effort cleanup - don't block session closure on failure
	if err := markSessionEnded(input.SessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark session ended: %v\n", err)
	}
	return nil
}

// markSessionEnded transitions the session to ENDED phase via the state machine.
func markSessionEnded(sessionID string) error {
	state, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return nil // No state file, nothing to update
	}

	strategy.TransitionAndLog(state, session.EventSessionStop, session.TransitionContext{})

	now := time.Now()
	state.EndedAt = &now

	if err := strategy.SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	return nil
}

// stopHookSentinel is the string that appears in Claude Code's hook_progress
// transcript entry when it launches our stop hook. Used to detect that the
// transcript file has been fully flushed before we copy it.
const stopHookSentinel = "hooks claude-code stop"

// waitForTranscriptFlush polls the transcript file tail for the hook_progress
// sentinel entry that Claude Code writes when launching the stop hook.
// Once this entry appears in the file, all prior entries (assistant replies,
// tool results) are guaranteed to have been flushed.
//
// hookStartTime is the approximate time our process started, used to avoid
// matching stale sentinel entries from previous stop hook invocations.
//
// Falls back silently after a timeout — the transcript copy will proceed
// with whatever data is available.
func waitForTranscriptFlush(transcriptPath string, hookStartTime time.Time) {
	const (
		maxWait      = 3 * time.Second
		pollInterval = 50 * time.Millisecond
		tailBytes    = 4096 // Read last 4KB — sentinel is near the end
		maxSkew      = 2 * time.Second
	)

	logCtx := logging.WithComponent(context.Background(), "hooks")
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if checkStopSentinel(transcriptPath, tailBytes, hookStartTime, maxSkew) {
			logging.Debug(logCtx, "transcript flush sentinel found",
				slog.Duration("wait", time.Since(hookStartTime)),
			)
			return
		}
		time.Sleep(pollInterval)
	}
	// Timeout — proceed with whatever is on disk.
	logging.Warn(logCtx, "transcript flush sentinel not found within timeout, proceeding",
		slog.Duration("timeout", maxWait),
	)
}

// checkStopSentinel reads the tail of the transcript file and looks for a
// hook_progress entry containing the stop hook sentinel, with a timestamp
// close to hookStartTime.
func checkStopSentinel(path string, tailBytes int64, hookStartTime time.Time, maxSkew time.Duration) bool {
	f, err := os.Open(path) //nolint:gosec // path comes from agent hook input
	if err != nil {
		return false
	}
	defer f.Close()

	// Seek to tail
	info, err := f.Stat()
	if err != nil {
		return false
	}
	offset := info.Size() - tailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return false
	}

	// Scan lines from the tail for the sentinel
	lines := strings.Split(string(buf), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, stopHookSentinel) {
			continue
		}

		// Parse timestamp to check recency
		var entry struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			ts, err = time.Parse(time.RFC3339, entry.Timestamp)
			if err != nil {
				continue
			}
		}

		// Check timestamp is within skew window of our start time
		if ts.After(hookStartTime.Add(-maxSkew)) && ts.Before(hookStartTime.Add(maxSkew)) {
			return true
		}
	}
	return false
}
