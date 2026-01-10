// hooks_opencode_handlers.go contains OpenCode specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	// Read stdin
	stdinData, readErr := io.ReadAll(os.Stdin)
	if readErr != nil {
		return fmt.Errorf("failed to read stdin: %w", readErr)
	}

	// Skip on default branch for strategies that don't allow it
	skip, branchName := ShouldSkipOnDefaultBranchForStrategy()
	if skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil // Don't fail the hook, just skip
	}

	// Get the OpenCode agent specifically (not auto-detected agent)
	// This hook is called via "entire hooks opencode stop", so we know it's OpenCode
	ag, err := agent.Get(agent.AgentNameOpenCode)
	if err != nil {
		return fmt.Errorf("failed to get OpenCode agent: %w", err)
	}

	// Create a reader from the data we already read
	reader := bytes.NewReader(stdinData)

	// Parse hook input using agent interface
	input, err := ag.ParseHookInput(agent.HookStop, reader)
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

	// Get the Entire session ID from the agent transformation
	entireSessionID := ag.TransformSessionID(modelSessionID)

	// Get transcript path from RawData (OpenCode plugin exports it to .entire/opencode/sessions/)
	transcriptPath, ok := input.RawData["transcript_path"].(string)
	if !ok || transcriptPath == "" {
		return errors.New("transcript_path not found in hook input")
	}

	if !fileExists(transcriptPath) {
		return fmt.Errorf("transcript file not found: %s", transcriptPath)
	}

	// Read the session from OpenCode's exported data
	session, err := ag.ReadSession(input)
	if err != nil {
		return fmt.Errorf("failed to read session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Read OpenCode session from: %s\n", transcriptPath)

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

	// Copy transcript to metadata directory
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := copyFile(transcriptPath, logFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", sessionDir+"/"+paths.TranscriptFileName)

	// Parse OpenCode transcript
	transcriptLines, err := parseOpencodeTranscript(transcriptPath)
	if err != nil {
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Extract session title (shown as description in session list)
	sessionTitle := extractOpencodeSessionTitle(transcriptLines)
	if sessionTitle == "" {
		sessionTitle = "OpenCode session"
	}

	// Extract user prompts
	allPrompts := extractOpencodeUserPrompts(transcriptLines)
	promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)

	// Write session title as first line (used as description), then prompts
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

	// Extract modified files from transcript
	modifiedFiles, newFiles, deletedFiles := extractOpencodeModifiedFiles(transcriptLines)

	// Get current working directory (repo root)
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	// Filter and normalize paths (CLI responsibility)
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	relNewFiles := FilterAndNormalizePaths(newFiles, repoRoot)
	relDeletedFiles := FilterAndNormalizePaths(deletedFiles, repoRoot)

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
			fmt.Fprintf(os.Stderr, "  x %s\n", file)
		}
	}

	// Generate context summary
	contextContent := generateOpencodeContext(transcriptLines)
	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := os.WriteFile(contextFile, []byte(contextContent), 0o600); err != nil {
		return fmt.Errorf("failed to write context: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", sessionDir+"/"+paths.ContextFileName)

	// Build commit message from last prompt
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

	// Build save context with full metadata
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

	fmt.Fprintf(os.Stderr, "Created checkpoint for OpenCode session: %s\n", entireSessionID)

	// Store session data if the agent has a WriteSession method
	if err := ag.WriteSession(session); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write session: %v\n", err)
	}

	return nil
}
