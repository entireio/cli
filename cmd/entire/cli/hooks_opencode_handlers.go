// hooks_opencode_handlers.go contains OpenCode specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

	// Ensure strategy setup is in place (git hooks, gitignore, metadata branch).
	// Done here at turn start so hooks are installed before any mid-turn commits.
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
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
		resolved := ag.ResolveSessionFile(transcriptPath, sessionID)
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

// commitOpenCodeSession commits the session changes using the shared commit pipeline.
// OpenCode uses git status as primary file change detection (useGitStatusForModified=true)
// since it doesn't have reliable transcript-based file extraction.
func commitOpenCodeSession(ctx *openCodeSessionContext) error {
	return commitAgentSession(&agentCommitContext{
		sessionID:               ctx.sessionID,
		sessionDir:              ctx.sessionDir,
		sessionDirAbs:           ctx.sessionDirAbs,
		commitMessage:           ctx.commitMessage,
		transcriptPath:          ctx.transcriptPath,
		useGitStatusForModified: true,
		prompts:                 singlePrompt(ctx.userPrompt),
	})
}
