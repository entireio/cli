// hooks_openclaw_handlers.go contains OpenClaw specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// handleOpenClawSessionStart handles the SessionStart hook for OpenClaw.
func handleOpenClawSessionStart() error {
	return handleSessionStartCommon()
}

// handleOpenClawSessionEnd handles the SessionEnd hook for OpenClaw.
func handleOpenClawSessionEnd() error {
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
		return nil
	}

	if err := markSessionEnded(input.SessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark session ended: %v\n", err)
	}
	return nil
}

// handleOpenClawUserPromptSubmit captures initial state on user prompt submit.
func handleOpenClawUserPromptSubmit() error {
	hookData, err := parseAndLogHookInput()
	if err != nil {
		return err
	}

	if err := CapturePrePromptState(hookData.sessionID, hookData.input.SessionRef); err != nil {
		return err
	}

	strat := GetStrategy()
	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		agentType := hookData.agent.Type()
		if err := initializer.InitializeSession(hookData.sessionID, agentType, hookData.input.SessionRef, hookData.input.UserPrompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
		}
	}

	return nil
}

// handleOpenClawStop commits the session changes with metadata.
func handleOpenClawStop() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

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

	if repo, err := strategy.OpenRepository(); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(os.Stderr, "Entire: skipping checkpoint. Will activate after first commit.")
		return NewSilentError(strategy.ErrEmptyRepository)
	}

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
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

	// Load pre-prompt state
	preState, err := LoadPrePromptState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}

	var transcriptOffset int
	if preState != nil && preState.StepTranscriptStart > 0 {
		transcriptOffset = preState.StepTranscriptStart
		fmt.Fprintf(os.Stderr, "Pre-prompt state found: parsing transcript from line %d\n", transcriptOffset)
	} else {
		sessionState, loadErr := strategy.LoadSessionState(sessionID)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		}
		if sessionState != nil && sessionState.CheckpointTranscriptStart > 0 {
			transcriptOffset = sessionState.CheckpointTranscriptStart
			fmt.Fprintf(os.Stderr, "Session state found: parsing transcript from line %d\n", transcriptOffset)
		}
	}

	var transcript []transcriptLine
	var totalLines int
	if transcriptOffset > 0 {
		transcript, totalLines, err = parseTranscriptFromLine(transcriptPath, transcriptOffset)
		if err != nil {
			return fmt.Errorf("failed to parse transcript from line %d: %w", transcriptOffset, err)
		}
		fmt.Fprintf(os.Stderr, "Parsed %d new transcript lines (total: %d)\n", len(transcript), totalLines)
	} else {
		transcript, totalLines, err = parseTranscriptFromLine(transcriptPath, 0)
		if err != nil {
			return fmt.Errorf("failed to parse transcript: %w", err)
		}
	}

	// Extract prompts
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

	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	commitMessage := generateCommitMessage(lastPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", commitMessage)

	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	if preState != nil {
		fmt.Fprintf(os.Stderr, "Pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
	}

	changes, err := DetectFileChanges(preState.PreUntrackedFiles())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files were modified during this session\n")
		fmt.Fprintf(os.Stderr, "Skipping commit\n")
		transitionSessionTurnEnd(sessionID)
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

	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := createContextFileMinimal(contextFile, commitMessage, sessionID, promptFile, summaryFile, transcript); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	var agentType agent.AgentType
	if hookAgent, agentErr := GetCurrentHookAgent(); agentErr == nil {
		agentType = hookAgent.Type()
	}

	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.StepTranscriptStart
	}

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
	}

	if err := strat.SaveChanges(ctx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	if strat.Name() == strategy.StrategyNameAutoCommit {
		sessionState, loadErr := strategy.LoadSessionState(sessionID)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		}
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

	transitionSessionTurnEnd(sessionID)

	if err := CleanupPrePromptState(sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
	}

	return nil
}
