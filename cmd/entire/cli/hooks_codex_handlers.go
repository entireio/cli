// hooks_codex_handlers.go contains Codex CLI specific hook handler implementations.
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
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// handleCodexAgentTurnComplete handles the agent-turn-complete hook for Codex CLI.
// This is the only hook Codex supports, firing after each agent turn completes.
// It is equivalent to Claude Code's "Stop" hook — it commits session changes with metadata.
func handleCodexAgentTurnComplete() error {
	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		return fmt.Errorf("failed to get codex agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
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
		resolved := ag.ResolveSessionFile(transcriptPath, ag.ExtractAgentSessionID(sessionID))
		if fileExists(resolved) {
			transcriptPath = resolved
		}
	}

	if transcriptPath == "" || !fileExists(transcriptPath) {
		return fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	// Create session context
	ctx := &codexSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
		userPrompt:     input.UserPrompt,
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

	// Transition session ACTIVE → IDLE (equivalent to Claude's transitionSessionTurnEnd)
	transitionSessionTurnEnd(sessionID)

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

// commitCodexSession commits the session changes using the strategy.
func commitCodexSession(ctx *codexSessionContext) error {
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

	relModifiedFiles := FilterAndNormalizePaths(ctx.modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
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
	if err := createContextFileForCodex(contextFile, ctx.commitMessage, ctx.sessionID, ctx.userPrompt); err != nil {
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

	// Get agent type from the hook agent
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

// createContextFileForCodex creates a context.md file for Codex sessions.
func createContextFileForCodex(contextFile, commitMessage, sessionID, prompt string) error {
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
