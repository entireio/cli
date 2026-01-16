// hooks_geminicli_handlers.go contains Gemini CLI specific hook handler implementations.
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
	"entire.io/cli/cmd/entire/cli/agent/geminicli"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// ErrSessionSkipped is returned when a session should be skipped (e.g., due to concurrent warning).
var ErrSessionSkipped = errors.New("session skipped")

// geminiBlockingResponse represents a JSON response for Gemini CLI hooks.
// When decision is "block", Gemini CLI will block the current operation and show the reason to the user.
type geminiBlockingResponse struct {
	Decision      string `json:"decision"`
	Reason        string `json:"reason"`
	SystemMessage string `json:"systemMessage,omitempty"`
}

// outputGeminiBlockingResponse outputs a blocking JSON response to stdout for Gemini CLI hooks
// and exits with code 0. For BeforeAgent hooks, the JSON response with decision "block" tells
// Gemini CLI to block the operation - exit code 0 is required for the JSON to be parsed.
// This function does not return - it calls os.Exit(0) after outputting the response.
func outputGeminiBlockingResponse(reason string) {
	resp := geminiBlockingResponse{
		Decision:      "block",
		Reason:        reason,
		SystemMessage: "⚠️ Session blocked: " + reason,
	}
	// Output to stdout (Gemini reads hook output from stdout with exit code 0)
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding blocking response: %v\n", err)
	}
	os.Exit(0)
}

// checkConcurrentSessionsGemini checks for concurrent session conflicts for Gemini CLI.
// If a conflict is found, it outputs a Gemini-format blocking response and exits (via os.Exit).
// Returns true if the hook should be skipped (warning already shown), false to proceed normally.
// Note: This function may call os.Exit(0) and not return if a blocking response is needed.
func checkConcurrentSessionsGemini(entireSessionID string) bool {
	// Always use the Gemini agent for resume commands in Gemini hooks
	// (don't use GetAgent() which may return Claude based on settings)
	geminiAgent, err := agent.Get("gemini")
	if err != nil {
		// Fall back to default if Gemini agent not found (shouldn't happen)
		geminiAgent = agent.Default()
	}
	strat := GetStrategy()

	concurrentChecker, ok := strat.(strategy.ConcurrentSessionChecker)
	if !ok {
		return false // Strategy doesn't support concurrent checks
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
			return true
		}
		// Warning was shown but conflict is resolved (e.g., user committed)
		// Clear the flag and proceed normally
		if existingState != nil {
			existingState.ConcurrentWarningShown = false
			if saveErr := strategy.SaveSessionState(existingState); saveErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to clear concurrent warning flag: %v\n", saveErr)
			}
		}
		return false
	}

	if hasConflict {
		// First time seeing conflict - show warning
		// Include BaseCommit and WorktreePath so session state is complete if conflict later resolves
		repo, err := strategy.OpenRepository()
		if err != nil {
			// Output user-friendly error message via blocking response
			outputGeminiBlockingResponse(fmt.Sprintf("Failed to open git repository: %v\n\nPlease ensure you're in a git repository and try again.", err))
			// outputGeminiBlockingResponse calls os.Exit(0), so this line is never reached
			return true
		}
		head, err := repo.Head()
		if err != nil {
			// Output user-friendly error message via blocking response
			outputGeminiBlockingResponse(fmt.Sprintf("Failed to get git HEAD: %v\n\nPlease ensure the repository has at least one commit.", err))
			// outputGeminiBlockingResponse calls os.Exit(0), so this line is never reached
			return true
		}
		worktreePath, err := strategy.GetWorktreePath()
		if err != nil {
			// Non-fatal: proceed without worktree path
			worktreePath = ""
		}

		// Derive agent type from agent description (e.g., "Gemini CLI" from "Gemini CLI - ...")
		agentType := geminiAgent.Description()
		if idx := strings.Index(agentType, " - "); idx > 0 {
			agentType = agentType[:idx]
		}
		newState := &strategy.SessionState{
			SessionID:              entireSessionID,
			BaseCommit:             head.Hash().String(),
			WorktreePath:           worktreePath,
			ConcurrentWarningShown: true,
			StartedAt:              time.Now(),
			AgentType:              agentType,
		}
		if saveErr := strategy.SaveSessionState(newState); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save session state: %v\n", saveErr)
		}

		// Get resume command for the other session using the CONFLICTING session's agent type.
		// If the conflicting session is from a different agent (e.g., Claude when we're Gemini),
		// use that agent's resume command format. Otherwise, use our own format.
		var resumeCmd string
		if otherSession.AgentType != "" && otherSession.AgentType != agentType {
			// Different agent type - look up the conflicting agent
			if conflictingAgent, agentErr := agent.GetByAgentType(otherSession.AgentType); agentErr == nil {
				resumeCmd = conflictingAgent.FormatResumeCommand(conflictingAgent.ExtractAgentSessionID(otherSession.SessionID))
			}
		}
		// Fall back to Gemini agent if same type or couldn't get the conflicting agent
		if resumeCmd == "" {
			resumeCmd = geminiAgent.FormatResumeCommand(geminiAgent.ExtractAgentSessionID(otherSession.SessionID))
		}

		// Output blocking JSON response and exit
		// Message format matches Claude Code but with Gemini-specific instructions
		outputGeminiBlockingResponse("You have another active session with uncommitted changes. Please commit them first and then start a new Gemini session. If you continue here, your prompt and resulting changes will not be captured.\n\nTo resume the active session, close Gemini CLI and run: " + resumeCmd)
		// outputGeminiBlockingResponse calls os.Exit(0), so this line is never reached
		return true
	}

	return false
}

// handleGeminiSessionStart handles the SessionStart hook for Gemini CLI.
// It reads session info from stdin and sets it as the current session.
func handleGeminiSessionStart() error {
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
	logging.Info(logCtx, "gemini-session-start",
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

// handleGeminiSessionEnd handles the SessionEnd hook for Gemini CLI.
// This fires when the user explicitly exits the session (via "exit" or "logout" commands).
// Note: The primary checkpoint creation happens in AfterAgent (equivalent to Claude's Stop).
// SessionEnd serves as a cleanup/fallback - it will commit any uncommitted changes that
// weren't captured by AfterAgent (e.g., if the user exits mid-response).
func handleGeminiSessionEnd() error {
	// Note: Don't parse stdin here - commitWithMetadataGemini() does its own parsing
	// and stdin can only be read once. Logging happens inside parseGeminiSessionEnd().
	return commitWithMetadataGemini()
}

// geminiSessionContext holds parsed session data for Gemini commits.
type geminiSessionContext struct {
	entireSessionID string
	modelSessionID  string
	transcriptPath  string
	sessionDir      string
	sessionDirAbs   string
	transcriptData  []byte
	allPrompts      []string
	summary         string
	modifiedFiles   []string
	commitMessage   string
}

// parseGeminiSessionEnd parses the session-end hook input and validates transcript.
func parseGeminiSessionEnd() (*geminiSessionContext, error) {
	ag, err := GetAgent()
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
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
		modelSessionID = "unknown"
	}

	entireSessionID := currentSessionIDWithFallback(modelSessionID)

	// Check if this session was already warned about concurrent sessions
	// (checkConcurrentSessionsGemini handles this in BeforeAgent, but we also check here
	// in case the session was warned and user continued anyway)
	state, stateErr := strategy.LoadSessionState(entireSessionID)
	if stateErr == nil && state != nil && state.ConcurrentWarningShown {
		return nil, ErrSessionSkipped
	}

	transcriptPath := input.SessionRef
	if transcriptPath == "" || !fileExists(transcriptPath) {
		return nil, fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	return &geminiSessionContext{
		entireSessionID: entireSessionID,
		modelSessionID:  modelSessionID,
		transcriptPath:  transcriptPath,
	}, nil
}

// setupGeminiSessionDir creates session directory and copies transcript.
func setupGeminiSessionDir(ctx *geminiSessionContext) error {
	ctx.sessionDir = paths.SessionMetadataDir(ctx.modelSessionID)
	sessionDirAbs, err := paths.AbsPath(ctx.sessionDir)
	if err != nil {
		sessionDirAbs = ctx.sessionDir
	}
	ctx.sessionDirAbs = sessionDirAbs

	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := copyFile(ctx.transcriptPath, logFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", ctx.sessionDir+"/"+paths.TranscriptFileName)

	transcriptData, err := os.ReadFile(ctx.transcriptPath)
	if err != nil {
		return fmt.Errorf("failed to read transcript: %w", err)
	}
	ctx.transcriptData = transcriptData

	return nil
}

// extractGeminiMetadata extracts prompts, summary, and modified files from transcript.
func extractGeminiMetadata(ctx *geminiSessionContext) error {
	allPrompts, err := geminicli.ExtractAllUserPrompts(ctx.transcriptData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to extract prompts: %v\n", err)
	}
	ctx.allPrompts = allPrompts

	promptFile := filepath.Join(ctx.sessionDirAbs, paths.PromptFileName)
	promptContent := strings.Join(allPrompts, "\n\n---\n\n")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted %d prompt(s) to: %s\n", len(allPrompts), ctx.sessionDir+"/"+paths.PromptFileName)

	summary, err := geminicli.ExtractLastAssistantMessage(ctx.transcriptData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to extract summary: %v\n", err)
	}
	ctx.summary = summary

	summaryFile := filepath.Join(ctx.sessionDirAbs, paths.SummaryFileName)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted summary to: %s\n", ctx.sessionDir+"/"+paths.SummaryFileName)

	modifiedFiles, err := geminicli.ExtractModifiedFiles(ctx.transcriptData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to extract modified files: %v\n", err)
	}
	ctx.modifiedFiles = modifiedFiles

	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	ctx.commitMessage = generateCommitMessage(lastPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", ctx.commitMessage)

	return nil
}

// commitGeminiSession commits the session changes using the strategy.
func commitGeminiSession(ctx *geminiSessionContext) error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	preState, err := LoadPrePromptState(ctx.entireSessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}
	if preState != nil {
		fmt.Fprintf(os.Stderr, "Loaded pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
	}

	newFiles, err := ComputeNewFiles(preState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute new files: %v\n", err)
	}

	deletedFiles, err := ComputeDeletedFiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute deleted files: %v\n", err)
	}

	relModifiedFiles := FilterAndNormalizePaths(ctx.modifiedFiles, repoRoot)
	relNewFiles := FilterAndNormalizePaths(newFiles, repoRoot)
	relDeletedFiles := FilterAndNormalizePaths(deletedFiles, repoRoot)

	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files were modified during this session\n")
		fmt.Fprintf(os.Stderr, "Skipping commit\n")
		if cleanupErr := CleanupPrePromptState(ctx.entireSessionID); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", cleanupErr)
		}
		return nil
	}

	logFileChanges(relModifiedFiles, relNewFiles, relDeletedFiles)

	contextFile := filepath.Join(ctx.sessionDirAbs, paths.ContextFileName)
	if err := createContextFileForGemini(contextFile, ctx.commitMessage, ctx.entireSessionID, ctx.allPrompts, ctx.summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", ctx.sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	saveCtx := strategy.SaveContext{
		SessionID:      ctx.entireSessionID,
		ModifiedFiles:  relModifiedFiles,
		NewFiles:       relNewFiles,
		DeletedFiles:   relDeletedFiles,
		MetadataDir:    ctx.sessionDir,
		MetadataDirAbs: ctx.sessionDirAbs,
		CommitMessage:  ctx.commitMessage,
		TranscriptPath: ctx.transcriptPath,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
	}

	if err := strat.SaveChanges(saveCtx); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	if cleanupErr := CleanupPrePromptState(ctx.entireSessionID); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", cleanupErr)
	}

	fmt.Fprintf(os.Stderr, "Session saved successfully\n")
	return nil
}

// logFileChanges logs the modified, new, and deleted files to stderr.
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

// commitWithMetadataGemini commits the Gemini session changes with metadata.
// This is a Gemini-specific version that parses JSON transcripts correctly.
func commitWithMetadataGemini() error {
	if skip, branchName := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil
	}

	ctx, err := parseGeminiSessionEnd()
	if err != nil {
		if errors.Is(err, ErrSessionSkipped) {
			return nil // Skip signaled
		}
		return err
	}

	if err := setupGeminiSessionDir(ctx); err != nil {
		return err
	}

	if err := extractGeminiMetadata(ctx); err != nil {
		return err
	}

	return commitGeminiSession(ctx)
}

// createContextFileForGemini creates a context.md file for Gemini sessions.
func createContextFileForGemini(contextFile, commitMessage, sessionID string, prompts []string, summary string) error {
	var sb strings.Builder

	sb.WriteString("# Session Context\n\n")
	sb.WriteString(fmt.Sprintf("Session ID: %s\n", sessionID))
	sb.WriteString(fmt.Sprintf("Commit Message: %s\n\n", commitMessage))

	if len(prompts) > 0 {
		sb.WriteString("## Prompts\n\n")
		for i, p := range prompts {
			sb.WriteString(fmt.Sprintf("### Prompt %d\n\n%s\n\n", i+1, p))
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

// handleGeminiBeforeTool handles the BeforeTool hook for Gemini CLI.
// This is similar to Claude Code's PreToolUse hook but applies to all tools.
func handleGeminiBeforeTool() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Debug(logCtx, "gemini-before-tool",
		slog.String("hook", "before-tool"),
		slog.String("hook_type", "tool"),
		slog.String("model_session_id", input.SessionID),
		slog.String("tool_name", input.ToolName),
	)

	// For now, BeforeTool is mainly for logging and potential future use
	// We don't need to do anything special before tool execution
	return nil
}

// handleGeminiAfterTool handles the AfterTool hook for Gemini CLI.
// This is similar to Claude Code's PostToolUse hook but applies to all tools.
func handleGeminiAfterTool() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPostToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Debug(logCtx, "gemini-after-tool",
		slog.String("hook", "after-tool"),
		slog.String("hook_type", "tool"),
		slog.String("model_session_id", input.SessionID),
		slog.String("tool_name", input.ToolName),
	)

	// For now, AfterTool is mainly for logging
	// Future: Could be used for incremental checkpoints similar to Claude's PostTodo
	return nil
}

// handleGeminiBeforeAgent handles the BeforeAgent hook for Gemini CLI.
// This is equivalent to Claude Code's UserPromptSubmit - it fires when the user submits a prompt.
// We capture the initial state here so we can track what files were modified during the session.
// It also checks for concurrent sessions and blocks if another session has uncommitted changes.
func handleGeminiBeforeAgent() error {
	// Always use the Gemini agent for Gemini hooks (don't use GetAgent() which may
	// return Claude based on auto-detection in environments like VSCode)
	ag, err := agent.Get("gemini")
	if err != nil {
		return fmt.Errorf("failed to get gemini agent: %w", err)
	}

	// Parse hook input - BeforeAgent provides user prompt info similar to UserPromptSubmit
	input, err := ag.ParseHookInput(agent.HookUserPromptSubmit, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")

	// Log with prompt if available (Gemini provides the user's prompt in BeforeAgent)
	logArgs := []any{
		slog.String("hook", "before-agent"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	}
	if input.UserPrompt != "" {
		// Truncate long prompts for logging
		promptPreview := input.UserPrompt
		if len(promptPreview) > 100 {
			promptPreview = promptPreview[:100] + "..."
		}
		logArgs = append(logArgs, slog.String("prompt_preview", promptPreview))
	}
	logging.Info(logCtx, "gemini-before-agent", logArgs...)

	if input.SessionID == "" {
		return errors.New("no session_id in input")
	}

	// Get the Entire session ID
	entireSessionID := paths.EntireSessionID(input.SessionID)

	// Check for concurrent sessions before proceeding
	// This will output a blocking response and exit if there's a conflict
	if checkConcurrentSessionsGemini(entireSessionID) {
		return nil
	}

	// Capture pre-prompt state (same as Claude Code's captureInitialState)
	if err := CapturePrePromptState(entireSessionID); err != nil {
		return fmt.Errorf("failed to capture pre-prompt state: %w", err)
	}

	// If strategy implements SessionInitializer, call it to initialize session state
	strat := GetStrategy()
	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		// Use agent description, but trim to just the name part (before " - ")
		agentType := ag.Description()
		if idx := strings.Index(agentType, " - "); idx > 0 {
			agentType = agentType[:idx]
		}
		if initErr := initializer.InitializeSession(entireSessionID, agentType); initErr != nil {
			if handleErr := handleSessionInitErrors(ag, initErr); handleErr != nil {
				return handleErr
			}
		}
	}

	return nil
}

// handleGeminiAfterAgent handles the AfterAgent hook for Gemini CLI.
// This fires after the agent has finished processing and generated a response.
// This is equivalent to Claude Code's "Stop" hook - it commits the session changes with metadata.
// AfterAgent fires after EVERY user prompt/response cycle, making it the correct place
// for checkpoint creation (not SessionEnd, which only fires on explicit exit).
func handleGeminiAfterAgent() error {
	// Skip on default branch for strategies that don't allow it
	if skip, branchName := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Fprintf(os.Stderr, "Entire: skipping on branch '%s' - create a feature branch to use Entire tracking\n", branchName)
		return nil
	}

	// Always use Gemini agent for Gemini hooks
	ag, err := agent.Get("gemini")
	if err != nil {
		return fmt.Errorf("failed to get gemini agent: %w", err)
	}

	// Parse hook input using HookStop - AfterAgent provides the same data as Stop
	// (session_id, transcript_path) which is what we need for committing
	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "gemini-after-agent",
		slog.String("hook", "after-agent"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	modelSessionID := input.SessionID
	if modelSessionID == "" {
		modelSessionID = "unknown"
	}

	entireSessionID := currentSessionIDWithFallback(modelSessionID)

	// Skip if this session was warned about concurrent sessions
	state, stateErr := strategy.LoadSessionState(entireSessionID)
	if stateErr == nil && state != nil && state.ConcurrentWarningShown {
		return nil
	}

	transcriptPath := input.SessionRef
	if transcriptPath == "" || !fileExists(transcriptPath) {
		return fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	// Create session context and commit
	ctx := &geminiSessionContext{
		entireSessionID: entireSessionID,
		modelSessionID:  modelSessionID,
		transcriptPath:  transcriptPath,
	}

	if err := setupGeminiSessionDir(ctx); err != nil {
		return err
	}

	if err := extractGeminiMetadata(ctx); err != nil {
		return err
	}

	return commitGeminiSession(ctx)
}

// handleGeminiBeforeModel handles the BeforeModel hook for Gemini CLI.
// This fires before every LLM call (potentially multiple times per agent loop).
// Useful for logging/monitoring LLM requests.
func handleGeminiBeforeModel() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input - use HookPreToolUse as a generic hook type for now
	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Debug(logCtx, "gemini-before-model",
		slog.String("hook", "before-model"),
		slog.String("hook_type", "model"),
		slog.String("model_session_id", input.SessionID),
	)

	// For now, BeforeModel is mainly for logging
	// Future: Could be used for request interception/modification
	return nil
}

// handleGeminiAfterModel handles the AfterModel hook for Gemini CLI.
// This fires after every LLM response (potentially multiple times per agent loop).
// Useful for logging/monitoring LLM responses.
func handleGeminiAfterModel() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPostToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Debug(logCtx, "gemini-after-model",
		slog.String("hook", "after-model"),
		slog.String("hook_type", "model"),
		slog.String("model_session_id", input.SessionID),
	)

	// For now, AfterModel is mainly for logging
	// Future: Could be used for response processing/analysis
	return nil
}

// handleGeminiBeforeToolSelection handles the BeforeToolSelection hook for Gemini CLI.
// This fires before the planner runs to select which tools to use.
func handleGeminiBeforeToolSelection() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Debug(logCtx, "gemini-before-tool-selection",
		slog.String("hook", "before-tool-selection"),
		slog.String("hook_type", "model"),
		slog.String("model_session_id", input.SessionID),
	)

	// For now, BeforeToolSelection is mainly for logging
	// Future: Could be used to modify tool availability
	return nil
}

// handleGeminiPreCompress handles the PreCompress hook for Gemini CLI.
// This fires before chat history compression - useful for backing up transcript.
func handleGeminiPreCompress() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookSessionStart, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Info(logCtx, "gemini-pre-compress",
		slog.String("hook", "pre-compress"),
		slog.String("hook_type", "session"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	// PreCompress is important for ensuring we capture the transcript before compression
	// The transcript_path gives us access to the full conversation before it's compressed
	// Future: Could automatically backup/checkpoint the transcript here
	return nil
}

// handleGeminiNotification handles the Notification hook for Gemini CLI.
// This fires on notification events (errors, warnings, info).
func handleGeminiNotification() error {
	// Get the agent for hook input parsing
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookSessionStart, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithComponent(context.Background(), "hooks")
	logging.Debug(logCtx, "gemini-notification",
		slog.String("hook", "notification"),
		slog.String("hook_type", "notification"),
		slog.String("model_session_id", input.SessionID),
	)

	// For now, Notification is mainly for logging
	// Future: Could be used for error tracking/alerting
	return nil
}
