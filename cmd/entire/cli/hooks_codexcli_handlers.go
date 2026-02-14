// hooks_codexcli_handlers.go contains Codex CLI specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
//
// Codex CLI only supports a single hook: agent-turn-complete (via its notify config).
// Unlike Claude Code (7 hooks) or Gemini CLI (10 hooks), Codex only fires
// a notify command after each turn completes. This handler captures state,
// extracts metadata, and commits in a single pass.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codexcli"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// codexSessionContext holds parsed session data for Codex commits.
type codexSessionContext struct {
	sessionID      string
	transcriptPath string
	sessionDir     string
	sessionDirAbs  string
	transcriptData []byte
	allPrompts     []string
	summary        string
	modifiedFiles  []string
	commitMessage  string
}

// handleCodexTurnComplete handles the turn-complete hook for Codex CLI.
// This is the only hook Codex supports — it fires after each agent turn completes.
// It combines the work of before-agent (state capture) and after-agent (commit)
// into a single handler since Codex doesn't have separate hooks for these phases.
func handleCodexTurnComplete() error {
	// Always use the Codex agent for Codex hooks
	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		return fmt.Errorf("failed to get codex agent: %w", err)
	}

	// Parse hook input — Codex sends a JSON payload via stdin
	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "codex-turn-complete",
		slog.String("hook", "turn-complete"),
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
		// Try to resolve transcript from session directory
		if transcriptPath == "" {
			transcriptPath, err = resolveCodexTranscript(ag, sessionID)
			if err != nil || transcriptPath == "" {
				return fmt.Errorf("transcript file not found for session %s", sessionID)
			}
		} else {
			return fmt.Errorf("transcript file not found: %s", transcriptPath)
		}
	}

	// Early check: bail out if the repo has no commits yet.
	if repo, repoErr := strategy.OpenRepository(); repoErr == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(os.Stderr, "Entire: skipping checkpoint. Will activate after first commit.")
		return NewSilentError(strategy.ErrEmptyRepository)
	}

	// Create session context and commit
	ctx := &codexSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
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

	// Transition session ACTIVE → IDLE
	transitionSessionTurnEnd(sessionID)

	return nil
}

// resolveCodexTranscript attempts to find the transcript file for a Codex session.
func resolveCodexTranscript(ag agent.Agent, sessionID string) (string, error) {
	sessionDir, err := ag.GetSessionDir("")
	if err != nil {
		return "", fmt.Errorf("failed to get session dir: %w", err)
	}

	transcriptPath := ag.ResolveSessionFile(sessionDir, sessionID)
	if transcriptPath != "" && fileExists(transcriptPath) {
		return transcriptPath, nil
	}

	return "", errors.New("could not resolve transcript file")
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

// extractCodexMetadata extracts prompts, summary, and modified files from transcript.
func extractCodexMetadata(ctx *codexSessionContext) error {
	// Parse the JSONL transcript
	lines, err := codexcli.ParseTranscript(ctx.transcriptData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to parse transcript: %v\n", err)
	}

	// Extract all user prompts
	allPrompts := codexcli.ExtractAllUserPrompts(lines)
	ctx.allPrompts = allPrompts

	promptFile := filepath.Join(ctx.sessionDirAbs, paths.PromptFileName)
	promptContent := strings.Join(allPrompts, "\n\n---\n\n")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted %d prompt(s) to: %s\n", len(allPrompts), ctx.sessionDir+"/"+paths.PromptFileName)

	// Extract last assistant message as summary
	summary := codexcli.ExtractLastAssistantMessage(lines)
	ctx.summary = summary

	summaryFile := filepath.Join(ctx.sessionDirAbs, paths.SummaryFileName)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted summary to: %s\n", ctx.sessionDir+"/"+paths.SummaryFileName)

	// Extract modified files from exec_command tool calls
	modifiedFiles := codexcli.ExtractModifiedFiles(lines)
	ctx.modifiedFiles = modifiedFiles

	// Generate commit message from the last prompt
	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	ctx.commitMessage = generateCommitMessage(lastPrompt)
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
		fmt.Fprintf(os.Stderr, "Loaded pre-prompt state: %d pre-existing untracked files, start message index: %d\n",
			len(preState.UntrackedFiles), preState.StartMessageIndex)
	}

	// Get transcript position from pre-prompt state
	var startMessageIndex int
	if preState != nil {
		startMessageIndex = preState.StartMessageIndex
	}

	// Calculate token usage for this turn (Codex-specific)
	var tokenUsage *agent.TokenUsage
	if ctx.transcriptPath != "" {
		usage, tokenErr := codexcli.CalculateTokenUsageFromFile(ctx.transcriptPath, startMessageIndex)
		if tokenErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to calculate token usage: %v\n", tokenErr)
		} else if usage != nil && usage.APICallCount > 0 {
			tokenUsage = usage
			fmt.Fprintf(os.Stderr, "Token usage for this checkpoint: input=%d, output=%d, cache_read=%d, api_calls=%d\n",
				tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.CacheReadTokens, tokenUsage.APICallCount)
		}
	}

	// Compute new and deleted files (single git status call)
	changes, err := DetectFileChanges(preState.PreUntrackedFiles())
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

	contextFile := filepath.Join(ctx.sessionDirAbs, paths.ContextFileName)
	if err := createContextFileForCodex(contextFile, ctx.commitMessage, ctx.sessionID, ctx.allPrompts, ctx.summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", ctx.sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()

	// Get agent type — we're in "entire hooks codex turn-complete", so it's Codex CLI
	hookAgent, agentErr := GetCurrentHookAgent()
	if agentErr != nil {
		return fmt.Errorf("failed to get agent: %w", agentErr)
	}
	agentType := hookAgent.Type()

	// Get transcript identifier at start from pre-prompt state
	var transcriptIdentifierAtStart string
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
	}

	saveCtx := strategy.SaveContext{
		SessionID:                ctx.sessionID,
		ModifiedFiles:            relModifiedFiles,
		NewFiles:                 relNewFiles,
		DeletedFiles:             relDeletedFiles,
		MetadataDir:              ctx.sessionDir,
		MetadataDirAbs:           ctx.sessionDirAbs,
		CommitMessage:            ctx.commitMessage,
		TranscriptPath:           ctx.transcriptPath,
		AuthorName:               author.Name,
		AuthorEmail:              author.Email,
		AgentType:                agentType,
		StepTranscriptStart:      startMessageIndex,
		StepTranscriptIdentifier: transcriptIdentifierAtStart,
		TokenUsage:               tokenUsage,
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
func createContextFileForCodex(contextFile, commitMessage, sessionID string, prompts []string, summary string) error {
	var sb strings.Builder

	sb.WriteString("# Session Context\n\n")
	sb.WriteString("Agent: Codex CLI\n")
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
