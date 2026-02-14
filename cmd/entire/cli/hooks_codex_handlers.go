// hooks_codex_handlers.go contains Codex CLI specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// handleCodexAgentTurnComplete handles the agent-turn-complete hook for Codex CLI.
// This is the only hook Codex supports, firing after each agent turn completes.
// It is equivalent to Claude Code's "Stop" hook — it commits session changes with metadata.
func handleCodexAgentTurnComplete() error {
	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		return fmt.Errorf("failed to get codex agent: %w", err)
	}

	// Codex sends its notify payload as the last positional argument (not stdin).
	// See codex-rs/hooks/src/user_notification.rs: command.arg(notify_payload), stdin(Stdio::null())
	var reader io.Reader = os.Stdin
	if args := GetCurrentHookArgs(); len(args) > 0 {
		reader = strings.NewReader(args[len(args)-1])
	}

	input, err := ag.ParseHookInput(agent.HookStop, reader)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "codex-agent-turn-complete",
		slog.String("hook", "agent-turn-complete"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Resolve transcript path from session directory
	transcriptPath := input.SessionRef
	if transcriptPath != "" && sessionID != unknownSessionID {
		resolved := ag.ResolveSessionFile(transcriptPath, sessionID)
		if fileExistsAndIsRegular(resolved) {
			transcriptPath = resolved
		}
	}

	if transcriptPath == "" || !fileExistsAndIsRegular(transcriptPath) {
		return fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	// Create session context
	ctx := &codexSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
		userPrompt:     input.UserPrompt,
	}

	// Ensure strategy setup is in place (git hooks, gitignore, metadata branch).
	// Codex has no separate turn-start hook, so we do this here before committing.
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	if err := setupCodexSessionDir(ctx); err != nil {
		return err
	}

	if err := extractCodexMetadata(ctx); err != nil {
		return err
	}

	if err := commitCodexSession(ctx); err != nil {
		return err
	}

	// Transition session ACTIVE → IDLE (equivalent to Claude's transitionSessionTurnEnd).
	// Note: Codex only supports one hook (agent-turn-complete), so the session state
	// machine is never initialized (no session-start or turn-start events). This call
	// will find no state and silently return. Session phase tracking (and features that
	// depend on it like deferred condensation) are not available for Codex sessions.
	transitionSessionTurnEnd(sessionID)

	// Capture post-turn state for the next turn's file change detection.
	// Codex has no "before turn" hook, so we capture state after each turn completes.
	// This gives the next turn's handler an accurate baseline of untracked files.
	// The transcript path is not used for offset tracking in Codex, so pass empty.
	if err := CapturePrePromptState(sessionID, ""); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to capture state for next turn: %v\n", err)
	}

	return nil
}

// codexSessionContext holds parsed session data for Codex commits.
type codexSessionContext struct {
	sessionID      string
	transcriptPath string
	sessionDir     string
	sessionDirAbs  string
	transcriptData []byte
	userPrompt     string
	modifiedFiles  []string
	commitMessage  string
}

// setupCodexSessionDir creates session directory and copies transcript.
func setupCodexSessionDir(ctx *codexSessionContext) error {
	ctx.sessionDir = paths.SessionMetadataDirFromSessionID(ctx.sessionID)
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

// extractCodexMetadata extracts prompts, modified files, and commit message from transcript.
func extractCodexMetadata(ctx *codexSessionContext) error {
	// Extract modified files from Codex JSONL transcript
	ctx.modifiedFiles = codex.ExtractModifiedFiles(ctx.transcriptData)

	// Write prompt file (Codex provides user prompt in the notify payload)
	promptFile := filepath.Join(ctx.sessionDirAbs, paths.PromptFileName)
	if ctx.userPrompt != "" {
		if err := os.WriteFile(promptFile, []byte(ctx.userPrompt), 0o600); err != nil {
			return fmt.Errorf("failed to write prompt file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Extracted prompt to: %s\n", ctx.sessionDir+"/"+paths.PromptFileName)
	}

	ctx.commitMessage = generateCommitMessage(ctx.userPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", ctx.commitMessage)

	return nil
}

// commitCodexSession commits the session changes using the shared commit pipeline.
func commitCodexSession(ctx *codexSessionContext) error {
	return commitAgentSession(&agentCommitContext{
		sessionID:               ctx.sessionID,
		sessionDir:              ctx.sessionDir,
		sessionDirAbs:           ctx.sessionDirAbs,
		commitMessage:           ctx.commitMessage,
		transcriptPath:          ctx.transcriptPath,
		transcriptModifiedFiles: ctx.modifiedFiles,
		prompts:                 singlePrompt(ctx.userPrompt),
	})
}
