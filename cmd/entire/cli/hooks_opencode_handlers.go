// hooks_opencode_handlers.go contains OpenCode specific hook handler implementations.
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

// handleOpenCodeSessionCreated handles the session-created hook for OpenCode.
// This fires when a new OpenCode session starts, equivalent to Claude's SessionStart.
func handleOpenCodeSessionCreated() error {
	return handleSessionStartCommon()
}

// handleOpenCodeSessionBusy handles the session-busy hook for OpenCode.
// This fires when OpenCode's session transitions to busy (agent starts processing a turn).
// Equivalent to Claude's "UserPromptSubmit" hook — it captures pre-prompt state
// (untracked files) so that the session-idle handler can accurately detect new files.
func handleOpenCodeSessionBusy() error {
	ag, err := agent.Get(agent.AgentNameOpenCode)
	if err != nil {
		return fmt.Errorf("failed to get opencode agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookUserPromptSubmit, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "opencode-session-busy",
		slog.String("hook", "session-busy"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Capture pre-prompt state (untracked files) before the agent starts working.
	// The transcript path is not used for OpenCode (no offset tracking), so pass empty.
	if err := CapturePrePromptState(sessionID, ""); err != nil {
		return fmt.Errorf("failed to capture pre-prompt state: %w", err)
	}

	return nil
}

// handleOpenCodeSessionIdle handles the session-idle hook for OpenCode.
// This fires when OpenCode's session transitions to idle (agent finished processing).
// Equivalent to Claude's "Stop" hook — it commits session changes with metadata.
func handleOpenCodeSessionIdle() error {
	ag, err := agent.Get(agent.AgentNameOpenCode)
	if err != nil {
		return fmt.Errorf("failed to get opencode agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "opencode-session-idle",
		slog.String("hook", "session-idle"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Resolve transcript path
	transcriptPath := input.SessionRef
	if transcriptPath != "" && sessionID != unknownSessionID {
		resolved := ag.ResolveSessionFile(transcriptPath, ag.ExtractAgentSessionID(sessionID))
		if fileExistsAndIsRegular(resolved) {
			transcriptPath = resolved
		}
	}

	if transcriptPath == "" || !fileExistsAndIsRegular(transcriptPath) {
		// OpenCode sessions may not always have a transcript file accessible
		// from the filesystem. Continue without transcript to still capture
		// git status changes.
		logging.Debug(logCtx, "no transcript file found, continuing with git status only",
			slog.String("transcript_path", transcriptPath),
		)
	}

	ctx := &openCodeSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
		userPrompt:     input.UserPrompt,
	}

	if err := setupOpenCodeSessionDir(ctx); err != nil {
		return err
	}

	if err := commitOpenCodeSession(ctx); err != nil {
		return err
	}

	// Transition session ACTIVE → IDLE
	transitionSessionTurnEnd(sessionID)

	return nil
}

// openCodeSessionContext holds parsed session data for OpenCode commits.
type openCodeSessionContext struct {
	sessionID      string
	transcriptPath string
	sessionDir     string
	sessionDirAbs  string
	userPrompt     string
	commitMessage  string
}

// setupOpenCodeSessionDir creates session directory and copies transcript if available.
func setupOpenCodeSessionDir(ctx *openCodeSessionContext) error {
	ctx.sessionDir = paths.SessionMetadataDirFromSessionID(ctx.sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx.sessionDir)
	if err != nil {
		sessionDirAbs = ctx.sessionDir
	}
	ctx.sessionDirAbs = sessionDirAbs

	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Copy transcript if available
	if ctx.transcriptPath != "" && fileExistsAndIsRegular(ctx.transcriptPath) {
		logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
		if err := copyFile(ctx.transcriptPath, logFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to copy transcript: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", ctx.sessionDir+"/"+paths.TranscriptFileName)
		}
	}

	// Write prompt file
	if ctx.userPrompt != "" {
		promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)
		if err := os.WriteFile(promptFile, []byte(ctx.userPrompt), 0o600); err != nil {
			return fmt.Errorf("failed to write prompt file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Extracted prompt to: %s\n", ctx.sessionDir+"/"+paths.PromptFileName)
	}

	ctx.commitMessage = generateCommitMessage(ctx.userPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", ctx.commitMessage)

	return nil
}

// commitOpenCodeSession commits the session changes using the strategy.
func commitOpenCodeSession(ctx *openCodeSessionContext) error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	preState, err := LoadPrePromptState(ctx.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}
	if preState != nil {
		fmt.Fprintf(os.Stderr, "Loaded pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
	}

	// Compute new and deleted files (single git status call)
	var preUntracked []string
	if preState != nil {
		preUntracked = preState.PreUntrackedFiles()
	}
	changes, err := DetectFileChanges(preUntracked)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// OpenCode sessions: use git status as primary file change detection
	var relNewFiles, relDeletedFiles []string
	var relModifiedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
		relModifiedFiles = FilterAndNormalizePaths(changes.Modified, repoRoot)
	}

	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files were modified during this session\n")
		fmt.Fprintf(os.Stderr, "Skipping commit\n")
		if cleanupErr := CleanupPrePromptState(ctx.sessionID); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", cleanupErr)
		}
		return nil
	}

	logFileChanges(relModifiedFiles, relNewFiles, relDeletedFiles)

	// Create context file
	contextFile := filepath.Join(ctx.sessionDirAbs, paths.ContextFileName)
	if err := createContextFileForOpenCode(contextFile, ctx.commitMessage, ctx.sessionID, ctx.userPrompt); err != nil {
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

	hookAgent, agentErr := GetCurrentHookAgent()
	if agentErr != nil {
		return fmt.Errorf("failed to get agent: %w", agentErr)
	}
	agentType := hookAgent.Type()

	saveCtx := strategy.SaveContext{
		SessionID:      ctx.sessionID,
		ModifiedFiles:  relModifiedFiles,
		NewFiles:       relNewFiles,
		DeletedFiles:   relDeletedFiles,
		MetadataDir:    ctx.sessionDir,
		MetadataDirAbs: ctx.sessionDirAbs,
		CommitMessage:  ctx.commitMessage,
		TranscriptPath: ctx.transcriptPath,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
		AgentType:      agentType,
	}

	if err := strat.SaveChanges(saveCtx); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	if cleanupErr := CleanupPrePromptState(ctx.sessionID); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", cleanupErr)
	}

	fmt.Fprintf(os.Stderr, "Session saved successfully\n")
	return nil
}

// createContextFileForOpenCode creates a context.md file for OpenCode sessions.
func createContextFileForOpenCode(contextFile, commitMessage, sessionID, prompt string) error {
	var sb strings.Builder

	sb.WriteString("# Session Context\n\n")
	sb.WriteString(fmt.Sprintf("Session ID: %s\n", sessionID))
	sb.WriteString(fmt.Sprintf("Commit Message: %s\n\n", commitMessage))

	if prompt != "" {
		sb.WriteString("## Prompt\n\n")
		sb.WriteString(prompt)
		sb.WriteString("\n")
	}

	if err := os.WriteFile(contextFile, []byte(sb.String()), 0o600); err != nil {
		return fmt.Errorf("failed to write context file: %w", err)
	}
	return nil
}
