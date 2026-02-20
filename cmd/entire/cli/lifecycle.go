// lifecycle.go implements the generic lifecycle event dispatcher.
// It routes normalized events from any agent to the appropriate framework actions.
//
// The dispatcher inverts the current flow from "agent handler calls framework functions"
// to "framework dispatcher calls agent methods." Agents are passive data providers;
// the dispatcher handles all orchestration: state transitions, strategy calls,
// file change detection, metadata generation.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// DispatchLifecycleEvent routes a normalized lifecycle event to the appropriate handler.
// Returns nil if the event was handled successfully.
func DispatchLifecycleEvent(ag agent.Agent, event *agent.Event) error {
	if ag == nil {
		return errors.New("agent cannot be nil")
	}
	if event == nil {
		return errors.New("event cannot be nil")
	}

	switch event.Type {
	case agent.SessionStart:
		return handleLifecycleSessionStart(ag, event)
	case agent.TurnStart:
		return handleLifecycleTurnStart(ag, event)
	case agent.TurnEnd:
		return handleLifecycleTurnEnd(ag, event)
	case agent.Compaction:
		return handleLifecycleCompaction(ag, event)
	case agent.SessionEnd:
		return handleLifecycleSessionEnd(ag, event)
	case agent.SubagentStart:
		return handleLifecycleSubagentStart(ag, event)
	case agent.SubagentEnd:
		return handleLifecycleSubagentEnd(ag, event)
	default:
		return fmt.Errorf("unknown lifecycle event type: %d", event.Type)
	}
}

// handleLifecycleSessionStart handles session start: shows banner, checks concurrent sessions,
// fires state machine transition.
func handleLifecycleSessionStart(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "session-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
	)

	if event.SessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if err := validation.ValidateSessionID(event.SessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	// Build informational message
	message := "\n\nPowered by Entire:\n  This conversation will be linked to your next commit."

	// Check for concurrent sessions and append count if any
	strat := GetStrategy()
	if concurrentChecker, ok := strat.(strategy.ConcurrentSessionChecker); ok {
		if count, err := concurrentChecker.CountOtherActiveSessionsWithCheckpoints(event.SessionID); err == nil && count > 0 {
			message += fmt.Sprintf("\n  %d other active conversation(s) in this workspace will also be included.\n  Use 'entire status' for more information.", count)
		}
	}

	// Output informational message
	if event.ResponseMessage != "" {
		message = event.ResponseMessage
	}
	if err := outputHookResponse(message); err != nil {
		return err
	}

	// Fire EventSessionStart for the current session (if state exists).
	if state, loadErr := strategy.LoadSessionState(event.SessionID); loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state on start: %v\n", loadErr)
	} else if state != nil {
		if transErr := strategy.TransitionAndLog(state, session.EventSessionStart, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: session start transition failed: %v\n", transErr)
		}
		if saveErr := strategy.SaveSessionState(state); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update session state on start: %v\n", saveErr)
		}
	}

	return nil
}

// handleLifecycleTurnStart handles turn start: captures pre-prompt state,
// ensures strategy setup, initializes session.
func handleLifecycleTurnStart(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "turn-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
	)

	sessionID := event.SessionID
	if sessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	// Capture pre-prompt state (including transcript position via TranscriptAnalyzer)
	if err := CapturePrePromptState(ag, sessionID, event.SessionRef); err != nil {
		return err
	}

	// Ensure strategy setup and initialize session
	strat := GetStrategy()

	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		agentType := ag.Type()
		if err := initializer.InitializeSession(sessionID, agentType, event.SessionRef, event.Prompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
		}
	}

	return nil
}

// handleLifecycleTurnEnd handles turn end: validates transcript, extracts metadata,
// detects file changes, saves step + checkpoint, transitions phase.
//
//nolint:maintidx // high complexity due to sequential orchestration of 8 steps (validation, extraction, file detection, filtering, token calc, step save, phase transition, cleanup) - splitting would obscure the flow
func handleLifecycleTurnEnd(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "turn-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
	)

	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	transcriptRef := event.SessionRef
	if transcriptRef == "" {
		return errors.New("transcript file not specified")
	}
	if !fileExists(transcriptRef) {
		return fmt.Errorf("transcript file not found: %s", transcriptRef)
	}

	// Early check: bail out quickly if the repo has no commits yet.
	if repo, err := strategy.OpenRepository(); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(os.Stderr, "Entire: skipping checkpoint. Will activate after first commit.")
		return NewSilentError(strategy.ErrEmptyRepository)
	}

	// Create session metadata directory
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// If agent implements TranscriptPreparer, wait for transcript to be ready
	if preparer, ok := ag.(agent.TranscriptPreparer); ok {
		if err := preparer.PrepareTranscript(transcriptRef); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to prepare transcript: %v\n", err)
		}
	}

	// Copy transcript to session directory
	transcriptData, err := ag.ReadTranscript(transcriptRef)
	if err != nil {
		return fmt.Errorf("failed to read transcript: %w", err)
	}
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, transcriptData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", sessionDir+"/"+paths.TranscriptFileName)

	// Copy export JSON if it exists alongside the transcript (written by OpenCode plugin).
	// This is used by `opencode import` during resume/rewind to restore the session
	// into OpenCode's SQLite database with the original session ID.
	exportSrc := strings.TrimSuffix(transcriptRef, filepath.Ext(transcriptRef)) + ".export.json"
	if exportData, readErr := os.ReadFile(exportSrc); readErr == nil { //nolint:gosec // Path derived from agent hook
		exportDest := filepath.Join(sessionDirAbs, paths.ExportDataFileName)
		if writeErr := os.WriteFile(exportDest, exportData, 0o600); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write export data: %v\n", writeErr)
		}
	}

	// Load pre-prompt state (captured on TurnStart)
	preState, err := LoadPrePromptState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}

	// Determine transcript offset
	transcriptOffset := resolveTranscriptOffset(preState, sessionID)

	// Extract metadata via agent interface (prompts, summary, modified files)
	var allPrompts []string
	var summary string
	var modifiedFiles []string
	var newTranscriptPosition int

	// Compute subagents directory for agents that support subagent extraction.
	// Subagent transcripts live in <transcriptDir>/<modelSessionID>/subagents/
	subagentsDir := filepath.Join(filepath.Dir(transcriptRef), event.SessionID, "subagents")

	if analyzer, ok := ag.(agent.TranscriptAnalyzer); ok {
		// Extract prompts
		if prompts, promptErr := analyzer.ExtractPrompts(transcriptRef, transcriptOffset); promptErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to extract prompts: %v\n", promptErr)
		} else {
			allPrompts = prompts
		}

		// Extract summary
		if s, sumErr := analyzer.ExtractSummary(transcriptRef); sumErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to extract summary: %v\n", sumErr)
		} else {
			summary = s
		}

		// Extract modified files - prefer SubagentAwareExtractor if available to include subagent files
		if subagentExtractor, subOk := ag.(agent.SubagentAwareExtractor); subOk {
			if files, fileErr := subagentExtractor.ExtractAllModifiedFiles(transcriptRef, transcriptOffset, subagentsDir); fileErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to extract modified files (with subagents): %v\n", fileErr)
			} else {
				modifiedFiles = files
			}
			// Get position from basic analyzer
			if _, pos, posErr := analyzer.ExtractModifiedFilesFromOffset(transcriptRef, transcriptOffset); posErr == nil {
				newTranscriptPosition = pos
			}
		} else {
			// Fall back to basic extraction (main transcript only)
			if files, pos, fileErr := analyzer.ExtractModifiedFilesFromOffset(transcriptRef, transcriptOffset); fileErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to extract modified files: %v\n", fileErr)
			} else {
				modifiedFiles = files
				newTranscriptPosition = pos
			}
		}
	}

	// Write prompts file
	promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)
	promptContent := strings.Join(allPrompts, "\n\n---\n\n")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted %d prompt(s) to: %s\n", len(allPrompts), sessionDir+"/"+paths.PromptFileName)

	// Write summary file
	summaryFile := filepath.Join(sessionDirAbs, paths.SummaryFileName)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted summary to: %s\n", sessionDir+"/"+paths.SummaryFileName)

	// Generate commit message from last prompt
	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	commitMessage := generateCommitMessage(lastPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", commitMessage)

	// Get repo root for path normalization
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	var preUntrackedFiles []string
	if preState != nil {
		fmt.Fprintf(os.Stderr, "Pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
		preUntrackedFiles = preState.PreUntrackedFiles()
	}

	// Detect file changes via git status
	changes, err := DetectFileChanges(preUntrackedFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// Filter and normalize all paths
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	// Filter transcript-extracted files to exclude files already committed to HEAD.
	// When an agent commits files mid-turn, those files are condensed by PostCommit
	// and should not be re-added to FilesTouched by SaveStep. A file is "committed"
	// if it exists in HEAD with the same content as the working tree.
	relModifiedFiles = filterToUncommittedFiles(relModifiedFiles, repoRoot)

	// Check if there are any changes
	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files were modified during this session\n")
		fmt.Fprintf(os.Stderr, "Skipping commit\n")
		transitionSessionTurnEnd(sessionID)
		if cleanupErr := CleanupPrePromptState(sessionID); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", cleanupErr)
		}
		return nil
	}

	// Log file changes
	logFileChanges(relModifiedFiles, relNewFiles, relDeletedFiles)

	// Create context file
	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := createContextFile(contextFile, commitMessage, sessionID, allPrompts, summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", sessionDir+"/"+paths.ContextFileName)

	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get strategy and agent type
	strat := GetStrategy()
	agentType := ag.Type()

	// Get transcript position/identifier from pre-prompt state
	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.TranscriptOffset
	}

	// Calculate token usage - prefer SubagentAwareExtractor to include subagent tokens
	var tokenUsage *agent.TokenUsage
	if subagentExtractor, ok := ag.(agent.SubagentAwareExtractor); ok {
		usage, tokenErr := subagentExtractor.CalculateTotalTokenUsage(transcriptRef, transcriptLinesAtStart, subagentsDir)
		if tokenErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to calculate token usage (with subagents): %v\n", tokenErr)
		} else {
			tokenUsage = usage
		}
	} else if calculator, ok := ag.(agent.TokenCalculator); ok {
		// Fall back to basic token calculation (main transcript only)
		usage, tokenErr := calculator.CalculateTokenUsage(transcriptRef, transcriptLinesAtStart)
		if tokenErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to calculate token usage: %v\n", tokenErr)
		} else {
			tokenUsage = usage
		}
	}

	// Build fully-populated step context and delegate to strategy
	ctx := strategy.StepContext{
		SessionID:                sessionID,
		ModifiedFiles:            relModifiedFiles,
		NewFiles:                 relNewFiles,
		DeletedFiles:             relDeletedFiles,
		MetadataDir:              sessionDir,
		MetadataDirAbs:           sessionDirAbs,
		CommitMessage:            commitMessage,
		TranscriptPath:           transcriptRef,
		AuthorName:               author.Name,
		AuthorEmail:              author.Email,
		AgentType:                agentType,
		StepTranscriptIdentifier: transcriptIdentifierAtStart,
		StepTranscriptStart:      transcriptLinesAtStart,
		TokenUsage:               tokenUsage,
	}

	if err := strat.SaveStep(ctx); err != nil {
		return fmt.Errorf("failed to save step: %w", err)
	}

	// Update session state transcript position for auto-commit strategy
	if strat.Name() == strategy.StrategyNameAutoCommit && newTranscriptPosition > 0 {
		updateAutoCommitTranscriptPosition(sessionID, newTranscriptPosition)
	}

	// Transition session phase and cleanup
	transitionSessionTurnEnd(sessionID)
	if cleanupErr := CleanupPrePromptState(sessionID); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", cleanupErr)
	}

	return nil
}

// handleLifecycleCompaction handles context compaction: saves current progress
// but stays in ACTIVE phase (unlike TurnEnd which transitions to IDLE).
// Also resets the transcript offset since the transcript may be truncated.
func handleLifecycleCompaction(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "compaction",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	// Fire EventCompaction to trigger ActionCondenseIfFilesTouched (stays in ACTIVE)
	sessionID := event.SessionID
	sessionState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state for compaction: %v\n", loadErr)
	}
	if sessionState != nil {
		if transErr := strategy.TransitionAndLog(sessionState, session.EventCompaction, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: compaction transition failed: %v\n", transErr)
		}

		// Reset transcript offset since the transcript may be truncated/reorganized
		sessionState.CheckpointTranscriptStart = 0

		if saveErr := strategy.SaveSessionState(sessionState); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save session state after compaction: %v\n", saveErr)
		}
	}

	fmt.Fprintf(os.Stderr, "Context compaction: transcript offset reset\n")
	return nil
}

// handleLifecycleSessionEnd handles session end: marks the session as ended.
func handleLifecycleSessionEnd(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "session-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	if event.SessionID == "" {
		return nil // No session to update
	}

	if err := markSessionEnded(event.SessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark session ended: %v\n", err)
	}
	return nil
}

// handleLifecycleSubagentStart handles subagent start: captures pre-task state.
func handleLifecycleSubagentStart(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "subagent-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
	)

	// Log context
	fmt.Fprintf(os.Stderr, "[entire] Subagent started\n")
	fmt.Fprintf(os.Stderr, "  Session ID: %s\n", event.SessionID)
	fmt.Fprintf(os.Stderr, "  Tool Use ID: %s\n", event.ToolUseID)
	fmt.Fprintf(os.Stderr, "  Transcript: %s\n", event.SessionRef)

	// Capture pre-task state
	if err := CapturePreTaskState(event.ToolUseID); err != nil {
		return fmt.Errorf("failed to capture pre-task state: %w", err)
	}

	return nil
}

// handleLifecycleSubagentEnd handles subagent end: detects changes, saves task checkpoint.
func handleLifecycleSubagentEnd(ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "lifecycle"), ag.Name())
	logging.Info(logCtx, "subagent-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
		slog.String("subagent_id", event.SubagentID),
	)

	// Extract subagent type and description from tool input
	subagentType, taskDescription := ParseSubagentTypeAndDescription(event.ToolInput)

	// Determine subagent transcript path
	transcriptDir := filepath.Dir(event.SessionRef)
	var subagentTranscriptPath string
	if event.SubagentID != "" {
		subagentTranscriptPath = AgentTranscriptPath(transcriptDir, event.SubagentID)
		if !fileExists(subagentTranscriptPath) {
			subagentTranscriptPath = ""
		}
	}

	// Log context
	fmt.Fprintf(os.Stderr, "[entire] Subagent completed\n")
	fmt.Fprintf(os.Stderr, "  Session ID: %s\n", event.SessionID)
	fmt.Fprintf(os.Stderr, "  Tool Use ID: %s\n", event.ToolUseID)
	if event.SubagentID != "" {
		fmt.Fprintf(os.Stderr, "  Agent ID: %s\n", event.SubagentID)
	}
	if subagentTranscriptPath != "" {
		fmt.Fprintf(os.Stderr, "  Subagent Transcript: %s\n", subagentTranscriptPath)
	}

	// Extract modified files from subagent transcript
	var modifiedFiles []string
	if analyzer, ok := ag.(agent.TranscriptAnalyzer); ok {
		transcriptToScan := event.SessionRef
		if subagentTranscriptPath != "" {
			transcriptToScan = subagentTranscriptPath
		}
		if files, _, fileErr := analyzer.ExtractModifiedFilesFromOffset(transcriptToScan, 0); fileErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to extract modified files from subagent: %v\n", fileErr)
		} else {
			modifiedFiles = files
		}
	}

	// Load pre-task state and detect file changes
	preState, err := LoadPreTaskState(event.ToolUseID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-task state: %v\n", err)
	}
	var preUntrackedFiles []string
	if preState != nil {
		preUntrackedFiles = preState.PreUntrackedFiles()
	}
	changes, err := DetectFileChanges(preUntrackedFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// Get repo root and normalize paths
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

	// If no changes, skip
	if len(relModifiedFiles) == 0 && len(relNewFiles) == 0 && len(relDeletedFiles) == 0 {
		fmt.Fprintf(os.Stderr, "[entire] No file changes detected, skipping task checkpoint\n")
		_ = CleanupPreTaskState(event.ToolUseID) //nolint:errcheck // best-effort cleanup
		return nil
	}

	// Find checkpoint UUID from main transcript (best-effort)
	var checkpointUUID string
	// Use the existing CLI-level checkpoint UUID finder
	mainLines, _, _ := parseTranscriptForCheckpointUUID(event.SessionRef) //nolint:errcheck // best-effort
	if mainLines != nil {
		checkpointUUID, _ = FindCheckpointUUID(mainLines, event.ToolUseID)
	}

	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Build task checkpoint context
	strat := GetStrategy()
	agentType := ag.Type()

	ctx := strategy.TaskStepContext{
		SessionID:              event.SessionID,
		ToolUseID:              event.ToolUseID,
		AgentID:                event.SubagentID,
		ModifiedFiles:          relModifiedFiles,
		NewFiles:               relNewFiles,
		DeletedFiles:           relDeletedFiles,
		TranscriptPath:         event.SessionRef,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         checkpointUUID,
		AuthorName:             author.Name,
		AuthorEmail:            author.Email,
		SubagentType:           subagentType,
		TaskDescription:        taskDescription,
		AgentType:              agentType,
	}

	if err := strat.SaveTaskStep(ctx); err != nil {
		return fmt.Errorf("failed to save task step: %w", err)
	}

	_ = CleanupPreTaskState(event.ToolUseID) //nolint:errcheck // best-effort cleanup
	return nil
}

// --- Helper functions ---

// resolveTranscriptOffset determines the transcript offset to use for parsing.
// Prefers pre-prompt state, falls back to session state.
func resolveTranscriptOffset(preState *PrePromptState, sessionID string) int {
	if preState != nil && preState.TranscriptOffset > 0 {
		fmt.Fprintf(os.Stderr, "Pre-prompt state found: parsing transcript from offset %d\n", preState.TranscriptOffset)
		return preState.TranscriptOffset
	}

	// Fall back to session state (e.g., auto-commit strategy updates it after each save)
	sessionState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		return 0
	}
	if sessionState != nil && sessionState.CheckpointTranscriptStart > 0 {
		fmt.Fprintf(os.Stderr, "Session state found: parsing transcript from offset %d\n", sessionState.CheckpointTranscriptStart)
		return sessionState.CheckpointTranscriptStart
	}

	return 0
}

// updateAutoCommitTranscriptPosition updates the session state with the new transcript position
// for the auto-commit strategy.
func updateAutoCommitTranscriptPosition(sessionID string, newPosition int) {
	sessionState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		return
	}
	if sessionState == nil {
		sessionState = &strategy.SessionState{
			SessionID: sessionID,
		}
	}
	sessionState.CheckpointTranscriptStart = newPosition
	sessionState.StepCount++
	if updateErr := strategy.SaveSessionState(sessionState); updateErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update session state: %v\n", updateErr)
	} else {
		fmt.Fprintf(os.Stderr, "Updated session state: transcript position=%d, checkpoint=%d\n",
			newPosition, sessionState.StepCount)
	}
}

// createContextFile creates a context.md file for the session checkpoint.
// This is a unified version that works for all agents.
func createContextFile(contextFile, commitMessage, sessionID string, prompts []string, summary string) error {
	var sb strings.Builder

	sb.WriteString("# Session Context\n\n")
	fmt.Fprintf(&sb, "Session ID: %s\n", sessionID)
	fmt.Fprintf(&sb, "Commit Message: %s\n\n", commitMessage)

	if len(prompts) > 0 {
		sb.WriteString("## Prompts\n\n")
		for i, p := range prompts {
			fmt.Fprintf(&sb, "### Prompt %d\n\n%s\n\n", i+1, p)
		}
	}

	if summary != "" {
		sb.WriteString("## Summary\n\n")
		sb.WriteString(summary)
		sb.WriteString("\n")
	}

	if err := os.WriteFile(contextFile, []byte(sb.String()), 0o600); err != nil {
		return fmt.Errorf("failed to write context file: %w", err)
	}
	return nil
}

// parseTranscriptForCheckpointUUID is a thin wrapper around transcript parsing for checkpoint UUID lookup.
// Returns parsed transcript lines for use with FindCheckpointUUID.
func parseTranscriptForCheckpointUUID(transcriptPath string) ([]transcriptLine, int, error) {
	lines, total, err := transcript.ParseFromFileAtLine(transcriptPath, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("parsing transcript for checkpoint UUID: %w", err)
	}
	return lines, total, nil
}

// transitionSessionTurnEnd transitions the session phase to IDLE and dispatches turn-end actions.
func transitionSessionTurnEnd(sessionID string) {
	turnState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state for turn end: %v\n", loadErr)
		return
	}
	if turnState == nil {
		return
	}
	if err := strategy.TransitionAndLog(turnState, session.EventTurnEnd, session.TransitionContext{}, session.NoOpActionHandler{}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: turn-end transition failed: %v\n", err)
	}

	// Always dispatch to strategy for turn-end handling. The strategy reads
	// work items from state (e.g. TurnCheckpointIDs), not the action list.
	strat := GetStrategy()
	if handler, ok := strat.(strategy.TurnEndHandler); ok {
		if err := handler.HandleTurnEnd(turnState); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: turn-end action dispatch failed: %v\n", err)
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

	if transErr := strategy.TransitionAndLog(state, session.EventSessionStop, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: session stop transition failed: %v\n", transErr)
	}

	now := time.Now()
	state.EndedAt = &now

	if err := strategy.SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	return nil
}

// logFileChanges logs the files modified, created, and deleted during a session.
func logFileChanges(modified, newFiles, deleted []string) {
	fmt.Fprintf(os.Stderr, "Files modified during session (%d):\n", len(modified))
	for _, file := range modified {
		fmt.Fprintf(os.Stderr, "  - %s\n", file)
	}
	if len(newFiles) > 0 {
		fmt.Fprintf(os.Stderr, "New files created (%d):\n", len(newFiles))
		for _, file := range newFiles {
			fmt.Fprintf(os.Stderr, "  - %s\n", file)
		}
	}
	if len(deleted) > 0 {
		fmt.Fprintf(os.Stderr, "Files deleted (%d):\n", len(deleted))
		for _, file := range deleted {
			fmt.Fprintf(os.Stderr, "  - %s\n", file)
		}
	}
}
