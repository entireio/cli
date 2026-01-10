// hooks_opencode_handlers.go contains OpenCode specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// handleOpencodeSessionStart handles the SessionStart hook for OpenCode.
// It reads session info from stdin and sets it as the current session.
func handleOpencodeSessionStart() error {
	// Get the OpenCode agent specifically (not auto-detected agent)
	// This hook is called via "entire hooks opencode session-start", so we know it's OpenCode
	ag, err := agent.Get(agent.AgentNameOpenCode)
	if err != nil {
		return fmt.Errorf("failed to get OpenCode agent: %w", err)
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
	entireSessionID := ag.TransformSessionID(input.SessionID)

	// Write session ID to current_session file
	if err := paths.WriteCurrentSession(entireSessionID); err != nil {
		return fmt.Errorf("failed to set current session: %w", err)
	}

	fmt.Printf("Current session set to: %s\n", entireSessionID)
	return nil
}

// handleOpencodeStop handles the Stop hook for OpenCode.
// OpenCode plugin exports transcript before calling this hook, so we just need to
// create a checkpoint from the exported data.
func handleOpencodeStop() error {
	// Skip on default branch for strategies that don't allow it
	if skip, branchName := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil
	}

	// Parse and validate input
	input, transcriptPath, ag, err := parseOpencodeStopInput()
	if err != nil {
		return err
	}

	modelSessionID := input.SessionID
	if modelSessionID == "" {
		modelSessionID = unknownSessionID
	}
	entireSessionID := ag.TransformSessionID(modelSessionID)

	// Read session data
	session, err := ag.ReadSession(input)
	if err != nil {
		return fmt.Errorf("failed to read session: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Read OpenCode session from: %s\n", transcriptPath)

	// Setup strategy and session directory
	sessionDir, sessionDirAbs, err := setupOpencodeSession(modelSessionID, transcriptPath)
	if err != nil {
		return err
	}

	// Process transcript and extract metadata
	transcriptLines, err := parseOpencodeTranscript(transcriptPath)
	if err != nil {
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	if err := saveOpencodeSessionMetadata(transcriptLines, sessionDir, sessionDirAbs); err != nil {
		return err
	}

	// Extract and save file changes
	fileChanges, commitMessage, err := extractAndSaveOpencodeFileChanges(transcriptLines, modelSessionID)
	if err != nil {
		return err
	}

	// Save checkpoint
	if err := saveOpencodeCheckpoint(entireSessionID, sessionDir, sessionDirAbs, transcriptPath, commitMessage, fileChanges); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Created checkpoint for OpenCode session: %s\n", entireSessionID)

	// Store session data
	if err := ag.WriteSession(session); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write session: %v\n", err)
	}

	return nil
}

// parseOpencodeStopInput parses and validates the OpenCode stop hook input.
//
//nolint:ireturn // Returning agent.Agent interface is intentional for abstraction
func parseOpencodeStopInput() (*agent.HookInput, string, agent.Agent, error) {
	// Get the OpenCode agent
	ag, err := agent.Get(agent.AgentNameOpenCode)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to get OpenCode agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "stop",
		slog.String("hook", "stop"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	// Get transcript path from RawData
	transcriptPath, ok := input.RawData["transcript_path"].(string)
	if !ok || transcriptPath == "" {
		return nil, "", nil, errors.New("transcript_path not found in hook input")
	}

	if !fileExists(transcriptPath) {
		return nil, "", nil, fmt.Errorf("transcript file not found: %s", transcriptPath)
	}

	return input, transcriptPath, ag, nil
}

// setupOpencodeSession sets up the strategy and creates the session directory.
func setupOpencodeSession(modelSessionID, transcriptPath string) (string, string, error) {
	// Get the configured strategy
	strat := GetStrategy()

	// Ensure strategy setup is in place
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	// Create session metadata folder
	sessionDir := paths.SessionMetadataDir(modelSessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir // Fallback to relative
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return "", "", fmt.Errorf("failed to create session directory: %w", err)
	}

	// Copy transcript to metadata directory
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := copyFile(transcriptPath, logFile); err != nil {
		return "", "", fmt.Errorf("failed to copy transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", sessionDir+"/"+paths.TranscriptFileName)

	return sessionDir, sessionDirAbs, nil
}

// saveOpencodeSessionMetadata extracts and saves prompts and context from the transcript.
func saveOpencodeSessionMetadata(transcriptLines []opencodeTranscriptLine, sessionDir, sessionDirAbs string) error {
	// Extract session title
	sessionTitle := extractOpencodeSessionTitle(transcriptLines)
	if sessionTitle == "" {
		sessionTitle = "OpenCode session"
	}

	// Extract user prompts
	allPrompts := extractOpencodeUserPrompts(transcriptLines)
	promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)

	// Write session title and prompts
	var promptContent strings.Builder
	promptContent.WriteString("# ")
	promptContent.WriteString(sessionTitle)
	promptContent.WriteString("\n\n")
	if len(allPrompts) > 0 {
		promptContent.WriteString(strings.Join(allPrompts, "\n\n---\n\n"))
	}

	if err := os.WriteFile(promptFile, []byte(promptContent.String()), 0o600); err != nil {
		return fmt.Errorf("failed to write prompts: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted %d prompts to: %s\n", len(allPrompts), sessionDir+"/"+paths.PromptFileName)

	// Generate and save context
	contextContent := generateOpencodeContext(transcriptLines)
	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := os.WriteFile(contextFile, []byte(contextContent), 0o600); err != nil {
		return fmt.Errorf("failed to write context: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", sessionDir+"/"+paths.ContextFileName)

	return nil
}

// opencodeFileChanges holds the extracted file changes from an OpenCode session.
type opencodeFileChanges struct {
	ModifiedFiles []string
	NewFiles      []string
	DeletedFiles  []string
}

// extractAndSaveOpencodeFileChanges extracts file changes and generates a commit message.
func extractAndSaveOpencodeFileChanges(transcriptLines []opencodeTranscriptLine, modelSessionID string) (*opencodeFileChanges, string, error) {
	// Extract modified files from transcript
	modifiedFiles, newFiles, deletedFiles := extractOpencodeModifiedFiles(transcriptLines)

	// Get repo root for path normalization
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get repo root: %w", err)
	}

	// Filter and normalize paths
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	relNewFiles := FilterAndNormalizePaths(newFiles, repoRoot)
	relDeletedFiles := FilterAndNormalizePaths(deletedFiles, repoRoot)

	// Log file changes
	logFileChanges(relModifiedFiles, relNewFiles, relDeletedFiles)

	// Build commit message
	allPrompts := extractOpencodeUserPrompts(transcriptLines)
	var commitMessage string
	if len(allPrompts) > 0 {
		lastPrompt := allPrompts[len(allPrompts)-1]
		// Use first line of last prompt as commit message
		firstLine := strings.Split(lastPrompt, "\n")[0]
		if len(firstLine) > 72 {
			firstLine = firstLine[:69] + "..."
		}
		commitMessage = firstLine
	} else {
		commitMessage = "OpenCode session: " + modelSessionID
	}

	return &opencodeFileChanges{
		ModifiedFiles: relModifiedFiles,
		NewFiles:      relNewFiles,
		DeletedFiles:  relDeletedFiles,
	}, commitMessage, nil
}

// saveOpencodeCheckpoint saves the checkpoint using the configured strategy.
func saveOpencodeCheckpoint(entireSessionID, sessionDir, sessionDirAbs, transcriptPath, commitMessage string, fileChanges *opencodeFileChanges) error {
	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get the configured strategy
	strat := GetStrategy()

	// Build save context
	ctx := strategy.SaveContext{
		SessionID:      entireSessionID,
		ModifiedFiles:  fileChanges.ModifiedFiles,
		NewFiles:       fileChanges.NewFiles,
		DeletedFiles:   fileChanges.DeletedFiles,
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

	return nil
}
