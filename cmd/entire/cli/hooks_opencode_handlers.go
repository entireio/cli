package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// handleOpenCodePromptSubmit captures pre-prompt state.
func handleOpenCodePromptSubmit() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookUserPromptSubmit, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "prompt-submit",
		slog.String("hook", "prompt-submit"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", sessionID),
	)

	if err := CapturePrePromptState(sessionID, input.SessionRef); err != nil {
		return err
	}

	strat := GetStrategy()
	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		agentType := ag.Type()
		if err := initializer.InitializeSession(sessionID, agentType, input.SessionRef, input.UserPrompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
		}
	}

	return nil
}

// handleOpenCodeStop creates a checkpoint using OpenCode session info.
func handleOpenCodeStop() error {
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
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Session metadata dir
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Reconstruct transcript from OpenCode storage
	transcriptData, err := opencode.ReconstructTranscript(sessionID)
	if err != nil {
		// Log warning but continue - transcript is optional for basic checkpoint functionality
		fmt.Fprintf(os.Stderr, "Warning: failed to reconstruct transcript: %v\n", err)
	} else if len(transcriptData) > 0 {
		// Write transcript to metadata directory
		transcriptPath := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
		if err := os.WriteFile(transcriptPath, transcriptData, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write transcript: %v\n", err)
		}
	}

	// Parse transcript to extract modified files if we have it
	var transcriptLines []opencode.TranscriptLine
	if len(transcriptData) > 0 {
		var parseErr error
		transcriptLines, parseErr = opencode.ParseTranscript(transcriptData)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse transcript: %v\n", parseErr)
		}
	}

	// Load pre-prompt state if available
	preState, err := LoadPrePromptState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}

	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	newFiles, deletedFiles, err := ComputeFileChanges(preState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// Extract modified files from transcript if available
	var modifiedFiles []string
	if len(transcriptLines) > 0 {
		modifiedFiles = opencode.ExtractModifiedFiles(transcriptLines)
	}

	// Load session state to get the first prompt (stored during InitializeSession)
	sessionState, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", err)
	}

	// Get prompt from session state (first prompt stored during initialization)
	// or extract from transcript if not available
	lastPrompt := ""
	if sessionState != nil && sessionState.FirstPrompt != "" {
		lastPrompt = sessionState.FirstPrompt
	} else if len(transcriptLines) > 0 {
		lastPrompt = opencode.ExtractLastUserPrompt(transcriptLines)
	}

	// Write prompt to metadata directory for explain command
	if lastPrompt != "" {
		promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)
		if err := os.WriteFile(promptFile, []byte(lastPrompt), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write prompt file: %v\n", err)
		}
	}

	commitMessage := generateCommitMessage(lastPrompt)

	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	relNewFiles := FilterAndNormalizePaths(newFiles, repoRoot)
	relDeletedFiles := FilterAndNormalizePaths(deletedFiles, repoRoot)

	// Get git author for commit authorship
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	// Calculate token usage
	var tokenUsage *agent.TokenUsage
	if len(transcriptLines) > 0 {
		tokenUsage = opencode.CalculateTokenUsage(transcriptLines)
		if tokenUsage != nil && tokenUsage.APICallCount > 0 {
			fmt.Fprintf(os.Stderr, "Token usage: input=%d, output=%d, calls=%d\n",
				tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.APICallCount)
		}
	}

	ctx := strategy.SaveContext{
		SessionID:      sessionID,
		ModifiedFiles:  relModifiedFiles,
		NewFiles:       relNewFiles,
		DeletedFiles:   relDeletedFiles,
		MetadataDir:    sessionDir,
		MetadataDirAbs: sessionDirAbs,
		CommitMessage:  commitMessage,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
		AgentType:      ag.Type(),
		TokenUsage:     tokenUsage,
	}

	if preState != nil {
		ctx.StepTranscriptIdentifier = preState.LastTranscriptIdentifier
		ctx.StepTranscriptStart = preState.StepTranscriptStart
	}

	if err := strat.SaveChanges(ctx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	// Transition session phase: ACTIVE → IDLE (or ACTIVE_COMMITTED → IDLE with condensation)
	transitionSessionTurnEnd(sessionID)

	// Clean up pre-prompt state
	if err := CleanupPrePromptState(sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
	}

	return nil
}
