// hooks_claudecode_handlers.go contains Claude Code specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// parseHookInputWithType parses hook input from reader using the current hook agent and given hook type.
// Used by both Claude Code and Cursor handlers so each agent can parse its own payload format.
func parseHookInputWithType(hookType agent.HookType, reader io.Reader, logName string) (*hookInputData, error) {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}
	input, err := ag.ParseHookInput(hookType, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, logName,
		slog.String("hook", logName),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}
	return &hookInputData{agent: ag, input: input, sessionID: sessionID}, nil
}

// parseAndLogHookInput parses the hook input and sets up logging context (user-prompt-submit).
func parseAndLogHookInput() (*hookInputData, error) {
	return parseHookInputWithType(agent.HookUserPromptSubmit, os.Stdin, "user-prompt-submit")
}

// captureInitialStateFromInput runs capture-initial-state logic given already-parsed hook input.
func captureInitialStateFromInput(ag agent.Agent, input *agent.HookInput) error {
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}
	if err := CapturePrePromptState(sessionID, input.SessionRef); err != nil {
		return err
	}
	strat := GetStrategy()
	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		if err := initializer.InitializeSession(sessionID, ag.Type(), input.SessionRef, input.UserPrompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
		}
	}
	return nil
}

// captureInitialState captures the initial state on user prompt submit.
func captureInitialState() error {
	hookData, err := parseAndLogHookInput()
	if err != nil {
		return err
	}
	return captureInitialStateFromInput(hookData.agent, hookData.input)
}

// commitWithMetadata commits the session changes with metadata.
func commitWithMetadata() error {
	hookData, err := parseHookInputWithType(agent.HookStop, os.Stdin, "stop")
	if err != nil {
		return err
	}
	return commitWithMetadataFromInput(hookData.agent, hookData.input)
}

// commitWithMetadataFromInput runs commit/checkpoint logic given already-parsed stop hook input.
func commitWithMetadataFromInput(ag agent.Agent, input *agent.HookInput) error { //nolint:maintidx // large shared stop logic
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}
	transcriptPath := input.SessionRef
	if transcriptPath == "" || !fileExists(transcriptPath) {
		return fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
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

	// Wait for agent to flush the transcript file (Claude Code writes a sentinel; other agents may not).
	if ag.Type() == agent.AgentTypeClaudeCode {
		waitForTranscriptFlush(transcriptPath, time.Now())
	}

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

	// Ensure strategy setup is in place (auto-installs git hook, gitignore, etc. if needed)
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	agentType := ag.Type()

	// Get transcript position from pre-prompt state (captured at step/turn start)
	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.StepTranscriptStart
	}

	// Calculate token usage for this checkpoint (Claude Code specific; Cursor/others can be added later)
	var tokenUsage *agent.TokenUsage
	if ag.Type() == agent.AgentTypeClaudeCode && transcriptPath != "" {
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

// runPostTodoLogic runs the post-todo incremental checkpoint logic (shared by Claude Code and Cursor).
func runPostTodoLogic(ag agent.Agent, sessionID, transcriptPath, toolName, toolUseID string, toolInput []byte) {
	taskToolUseID, found := FindActivePreTaskFile()
	if !found {
		return
	}
	if skip, branchName := ShouldSkipOnDefaultBranch(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping incremental checkpoint on branch '%s'\n", branchName)
		return
	}
	changes, err := DetectFileChanges(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to detect changed files: %v\n", err)
		return
	}
	if len(changes.Modified) == 0 && len(changes.New) == 0 && len(changes.Deleted) == 0 {
		fmt.Fprintf(os.Stderr, "[entire] No file changes detected, skipping incremental checkpoint\n")
		return
	}
	author, err := GetGitAuthor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to get git author: %v\n", err)
		return
	}
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
		return
	}
	if sessionID == "" {
		sessionID = paths.ExtractSessionIDFromTranscriptPath(transcriptPath)
	}
	seq := GetNextCheckpointSequence(sessionID, taskToolUseID)
	todoContent := ExtractLastCompletedTodoFromToolInput(toolInput)
	if todoContent == "" {
		todoCount := CountTodosFromToolInput(toolInput)
		if todoCount > 0 {
			todoContent = fmt.Sprintf("Planning: %d todos", todoCount)
		}
	}
	ctx := strategy.TaskCheckpointContext{
		SessionID:           sessionID,
		ToolUseID:           taskToolUseID,
		ModifiedFiles:       changes.Modified,
		NewFiles:            changes.New,
		DeletedFiles:        changes.Deleted,
		TranscriptPath:      transcriptPath,
		AuthorName:          author.Name,
		AuthorEmail:         author.Email,
		IsIncremental:       true,
		IncrementalSequence: seq,
		IncrementalType:     toolName,
		IncrementalData:     toolInput,
		TodoContent:         todoContent,
		AgentType:           ag.Type(),
	}
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save incremental checkpoint: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "[entire] Created incremental checkpoint #%d for %s (task: %s)\n",
		seq, toolName, taskToolUseID[:min(12, len(taskToolUseID))])
}

// handlePostTodoFromInput runs post-todo incremental checkpoint logic given already-parsed hook input.
func handlePostTodoFromInput(ag agent.Agent, input *agent.HookInput) {
	toolName := "TodoWrite"
	if name, ok := input.RawData["tool_name"].(string); ok && name != "" {
		toolName = name
	}
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = paths.ExtractSessionIDFromTranscriptPath(input.SessionRef)
	}
	runPostTodoLogic(ag, sessionID, input.SessionRef, toolName, input.ToolUseID, input.ToolInput)
}

// handleClaudeCodePostTodo handles the PostToolUse[TodoWrite] hook for subagent checkpoints.
func handleClaudeCodePostTodo() error {
	input, err := parseSubagentCheckpointHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[TodoWrite] input: %w", err)
	}
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
	runPostTodoLogic(ag, input.SessionID, input.TranscriptPath, input.ToolName, input.ToolUseID, input.ToolInput)
	return nil
}

// handlePreTaskFromInput runs pre-task logic given already-parsed hook input.
func handlePreTaskFromInput(ag agent.Agent, input *agent.HookInput) error {
	taskInput := &TaskHookInput{
		SessionID:      input.SessionID,
		TranscriptPath: input.SessionRef,
		ToolUseID:      input.ToolUseID,
		ToolInput:      input.ToolInput,
	}
	logPreTaskHookContext(os.Stdout, taskInput)
	return CapturePreTaskState(input.ToolUseID)
}

// handleClaudeCodePreTask handles the PreToolUse[Task] hook
func handleClaudeCodePreTask() error {
	hookData, err := parseHookInputWithType(agent.HookPreToolUse, os.Stdin, "pre-task")
	if err != nil {
		return err
	}
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), hookData.agent.Name())
	logging.Info(logCtx, "pre-task",
		slog.String("hook", "pre-task"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", hookData.input.SessionID),
		slog.String("transcript_path", hookData.input.SessionRef),
		slog.String("tool_use_id", hookData.input.ToolUseID),
	)
	return handlePreTaskFromInput(hookData.agent, hookData.input)
}

// postTaskParams holds the inputs needed to run post-task checkpoint logic.
type postTaskParams struct {
	SessionID      string
	TranscriptPath string
	ToolUseID      string
	AgentID        string
	ToolInput      []byte
}

// runPostTaskLogic runs the post-task checkpoint logic (shared by Claude Code and Cursor).
func runPostTaskLogic(ag agent.Agent, p postTaskParams) error {
	subagentType, taskDescription := ParseSubagentTypeAndDescription(p.ToolInput)
	transcriptDir := filepath.Dir(p.TranscriptPath)
	var subagentTranscriptPath string
	if p.AgentID != "" {
		subagentTranscriptPath = AgentTranscriptPath(transcriptDir, p.AgentID)
		if !fileExists(subagentTranscriptPath) {
			subagentTranscriptPath = ""
		}
	}
	postInput := &PostTaskHookInput{
		TaskHookInput: TaskHookInput{SessionID: p.SessionID, TranscriptPath: p.TranscriptPath, ToolUseID: p.ToolUseID, ToolInput: p.ToolInput},
		AgentID:       p.AgentID,
		ToolInput:     p.ToolInput,
	}
	logPostTaskHookContext(os.Stdout, postInput, subagentTranscriptPath)

	var modifiedFiles []string
	if subagentTranscriptPath != "" {
		transcript, err := parseTranscript(subagentTranscriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse subagent transcript: %v\n", err)
		} else {
			modifiedFiles = extractModifiedFiles(transcript)
		}
	} else {
		transcript, err := parseTranscript(p.TranscriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse transcript: %v\n", err)
		} else {
			modifiedFiles = extractModifiedFiles(transcript)
		}
	}

	preState, err := LoadPreTaskState(p.ToolUseID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-task state: %v\n", err)
	}
	changes, err := DetectFileChanges(preState.PreUntrackedFiles())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}
	if len(relModifiedFiles) == 0 && len(relNewFiles) == 0 && len(relDeletedFiles) == 0 {
		fmt.Fprintf(os.Stderr, "[entire] No file changes detected, skipping task checkpoint\n")
		_ = CleanupPreTaskState(p.ToolUseID) //nolint:errcheck // best-effort
		return nil
	}
	transcript, _ := parseTranscript(p.TranscriptPath) //nolint:errcheck // best-effort
	checkpointUUID, _ := FindCheckpointUUID(transcript, p.ToolUseID)
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}
	ctx := strategy.TaskCheckpointContext{
		SessionID:              p.SessionID,
		ToolUseID:              p.ToolUseID,
		AgentID:                p.AgentID,
		ModifiedFiles:          relModifiedFiles,
		NewFiles:               relNewFiles,
		DeletedFiles:           relDeletedFiles,
		TranscriptPath:         p.TranscriptPath,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         checkpointUUID,
		AuthorName:             author.Name,
		AuthorEmail:            author.Email,
		SubagentType:           subagentType,
		TaskDescription:        taskDescription,
		AgentType:              ag.Type(),
	}
	if err := strat.SaveTaskCheckpoint(ctx); err != nil {
		return fmt.Errorf("failed to save task checkpoint: %w", err)
	}
	_ = CleanupPreTaskState(p.ToolUseID) //nolint:errcheck // best-effort
	return nil
}

// handlePostTaskFromInput runs post-task checkpoint logic given already-parsed hook input.
func handlePostTaskFromInput(ag agent.Agent, input *agent.HookInput) error {
	agentID := ""
	if id, ok := input.RawData["agent_id"].(string); ok {
		agentID = id
	}
	return runPostTaskLogic(ag, postTaskParams{
		SessionID:      input.SessionID,
		TranscriptPath: input.SessionRef,
		ToolUseID:      input.ToolUseID,
		AgentID:        agentID,
		ToolInput:      input.ToolInput,
	})
}

// handleClaudeCodePostTask handles the PostToolUse[Task] hook
func handleClaudeCodePostTask() error {
	input, err := parsePostTaskHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse[Task] input: %w", err)
	}
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}
	subagentType, _ := ParseSubagentTypeAndDescription(input.ToolInput)
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "post-task",
		slog.String("hook", "post-task"),
		slog.String("hook_type", "subagent"),
		slog.String("tool_use_id", input.ToolUseID),
		slog.String("agent_id", input.AgentID),
		slog.String("subagent_type", subagentType),
	)
	return runPostTaskLogic(ag, postTaskParams{
		SessionID:      input.SessionID,
		TranscriptPath: input.TranscriptPath,
		ToolUseID:      input.ToolUseID,
		AgentID:        input.AgentID,
		ToolInput:      input.ToolInput,
	})
}

// handleClaudeCodeSessionStart handles the SessionStart hook for Claude Code.
func handleClaudeCodeSessionStart() error {
	return handleSessionStartCommon()
}

// handleSessionEndFromInput runs session-end logic given already-parsed hook input.
func handleSessionEndFromInput(ag agent.Agent, input *agent.HookInput) error {
	if input.SessionID == "" {
		return nil
	}
	if err := markSessionEnded(input.SessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark session ended: %v\n", err)
	}
	return nil
}

// handleClaudeCodeSessionEnd handles the SessionEnd hook for Claude Code.
// This fires when the user explicitly closes the session.
// Updates the session state with EndedAt timestamp.
func handleClaudeCodeSessionEnd() error {
	hookData, err := parseHookInputWithType(agent.HookSessionEnd, os.Stdin, "session-end")
	if err != nil {
		return err
	}
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), hookData.agent.Name())
	logging.Info(logCtx, "session-end",
		slog.String("hook", "session-end"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", hookData.input.SessionID),
	)
	return handleSessionEndFromInput(hookData.agent, hookData.input)
}

// transitionSessionTurnEnd fires EventTurnEnd to move the session from
// ACTIVE → IDLE (or ACTIVE_COMMITTED → IDLE). Best-effort: logs warnings
// on failure rather than returning errors.
func transitionSessionTurnEnd(sessionID string) {
	turnState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state for turn end: %v\n", loadErr)
		return
	}
	if turnState == nil {
		return
	}
	remaining := strategy.TransitionAndLog(turnState, session.EventTurnEnd, session.TransitionContext{})

	// Dispatch strategy-specific actions (e.g., ActionCondense for ACTIVE_COMMITTED → IDLE)
	if len(remaining) > 0 {
		strat := GetStrategy()
		if handler, ok := strat.(strategy.TurnEndHandler); ok {
			if err := handler.HandleTurnEnd(turnState, remaining); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: turn-end action dispatch failed: %v\n", err)
			}
		}
	}

	if updateErr := strategy.SaveSessionState(turnState); updateErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update session phase on turn end: %v\n", updateErr)
	}
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
