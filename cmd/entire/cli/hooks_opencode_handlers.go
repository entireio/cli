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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// handleOpencodeSessionStart handles the SessionStart hook for OpenCode.
// Calls the shared session-start logic and ensures strategy setup.
// OpenCode has no separate per-turn "before-agent" hook, so the first turn
// will not have pre-prompt state; the stop handler re-captures state for
// subsequent turns.
func handleOpencodeSessionStart() error {
	if err := handleSessionStartCommon(); err != nil {
		return err
	}

	// Ensure strategy setup (git hooks, gitignore, metadata branch)
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	return nil
}

// handleOpencodeStop handles the Stop hook for OpenCode.
// This fires after the agent finishes processing a user prompt.
// Follows the Gemini handleGeminiAfterAgent pattern: self-contained commit logic
// with agent-specific transcript parsing.
func handleOpencodeStop() error {
	ag, err := agent.Get(agent.AgentNameOpenCode)
	if err != nil {
		return fmt.Errorf("failed to get opencode agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "opencode-stop",
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

	// Early check: bail out if the repo has no commits yet.
	if repo, err := strategy.OpenRepository(); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(os.Stderr, "Entire: skipping checkpoint. Will activate after first commit.")
		return NewSilentError(strategy.ErrEmptyRepository)
	}

	ctx := &opencodeSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
	}

	if err := setupOpencodeSessionDir(ctx); err != nil {
		return err
	}

	if err := extractOpencodeMetadata(ctx); err != nil {
		return err
	}

	if err := commitOpencodeSession(ctx); err != nil {
		return err
	}

	// Transition session ACTIVE → IDLE
	transitionSessionTurnEnd(sessionID)

	// Re-capture pre-prompt state so subsequent turns have a baseline
	if err := CaptureOpencodePrePromptState(sessionID, transcriptPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to re-capture pre-prompt state: %v\n", err)
	}

	return nil
}

// handleOpencodeTaskStart handles the TaskStart hook for OpenCode.
// Captures pre-task state for subagent tracking.
func handleOpencodeTaskStart() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "opencode-task-start",
		slog.String("hook", "task-start"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("tool_use_id", input.ToolUseID),
	)

	if input.ToolUseID == "" {
		return errors.New("no tool_use_id in task-start input")
	}

	if err := CapturePreTaskState(input.ToolUseID); err != nil {
		return fmt.Errorf("failed to capture pre-task state: %w", err)
	}

	return nil
}

// handleOpencodeTaskComplete handles the TaskComplete hook for OpenCode.
// Commits subagent task checkpoint with transcript data.
func handleOpencodeTaskComplete() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookPostToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "opencode-task-complete",
		slog.String("hook", "task-complete"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("tool_use_id", input.ToolUseID),
	)

	if input.ToolUseID == "" {
		return errors.New("no tool_use_id in task-complete input")
	}

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Load pre-task state
	preTaskState, err := LoadPreTaskState(input.ToolUseID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-task state: %v\n", err)
	}

	// Compute file changes
	changes, err := DetectFileChanges(preTaskState.PreUntrackedFiles())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to detect file changes: %v\n", err)
	}

	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	var relModifiedFiles, relNewFiles, relDeletedFiles []string
	if changes != nil {
		relModifiedFiles = FilterAndNormalizePaths(changes.Modified, repoRoot)
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintf(os.Stderr, "No files modified during task, skipping checkpoint\n")
		if cleanupErr := CleanupPreTaskState(input.ToolUseID); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-task state: %v\n", cleanupErr)
		}
		return nil
	}

	// Extract subagent type and description from tool input
	subagentType, description := ParseSubagentTypeAndDescription(input.ToolInput)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()
	agentType := ag.Type()

	saveCtx := strategy.TaskCheckpointContext{
		SessionID:       sessionID,
		ToolUseID:       input.ToolUseID,
		ModifiedFiles:   relModifiedFiles,
		NewFiles:        relNewFiles,
		DeletedFiles:    relDeletedFiles,
		TranscriptPath:  input.SessionRef,
		AuthorName:      author.Name,
		AuthorEmail:     author.Email,
		AgentType:       agentType,
		SubagentType:    subagentType,
		TaskDescription: description,
	}

	// Set subagent transcript if available
	if subagentPath, ok := input.RawData["subagent_transcript_path"].(string); ok && subagentPath != "" && fileExists(subagentPath) {
		saveCtx.SubagentTranscriptPath = subagentPath
	}

	if err := strat.SaveTaskCheckpoint(saveCtx); err != nil {
		return fmt.Errorf("failed to save task checkpoint: %w", err)
	}

	if cleanupErr := CleanupPreTaskState(input.ToolUseID); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-task state: %v\n", cleanupErr)
	}

	fmt.Fprintf(os.Stderr, "Task checkpoint saved\n")
	return nil
}

// opencodeSessionContext holds parsed session data for OpenCode commits.
type opencodeSessionContext struct {
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

// setupOpencodeSessionDir creates session directory and copies transcript.
func setupOpencodeSessionDir(ctx *opencodeSessionContext) error {
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

// extractOpencodeMetadata extracts prompts, summary, and modified files from transcript.
func extractOpencodeMetadata(ctx *opencodeSessionContext) error {
	allPrompts, err := opencode.ExtractAllUserPrompts(ctx.transcriptData)
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

	summary, err := opencode.ExtractLastAssistantMessage(ctx.transcriptData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to extract summary: %v\n", err)
	}
	ctx.summary = summary

	summaryFile := filepath.Join(ctx.sessionDirAbs, paths.SummaryFileName)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted summary to: %s\n", ctx.sessionDir+"/"+paths.SummaryFileName)

	ctx.modifiedFiles = opencode.ExtractModifiedFiles(ctx.transcriptData)

	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	ctx.commitMessage = generateCommitMessage(lastPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", ctx.commitMessage)

	return nil
}

// commitOpencodeSession commits the session changes using the strategy.
func commitOpencodeSession(ctx *opencodeSessionContext) error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	preState, err := LoadPrePromptState(ctx.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}
	if preState != nil {
		fmt.Fprintf(os.Stderr, "Loaded pre-prompt state: %d pre-existing untracked files, start line: %d\n", len(preState.UntrackedFiles), preState.StepTranscriptStart)
	}

	// Get transcript position from pre-prompt state
	var startLineIndex int
	if preState != nil {
		startLineIndex = preState.StepTranscriptStart
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
	if err := createContextFileForOpencode(contextFile, ctx.commitMessage, ctx.sessionID, ctx.allPrompts, ctx.summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", ctx.sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()

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

	// Calculate token usage for this checkpoint (OpenCode specific)
	var tokenUsage *agent.TokenUsage
	if ctx.transcriptPath != "" {
		usage, tokenErr := opencode.CalculateTokenUsageFromFile(ctx.transcriptPath, startLineIndex)
		if tokenErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to calculate token usage: %v\n", tokenErr)
		} else if usage != nil && usage.APICallCount > 0 {
			tokenUsage = usage
			fmt.Fprintf(os.Stderr, "Token usage for this checkpoint: input=%d, output=%d, cache_read=%d, api_calls=%d\n",
				tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.CacheReadTokens, tokenUsage.APICallCount)
		}
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
		StepTranscriptStart:      startLineIndex,
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

// createContextFileForOpencode creates a context.md file for OpenCode sessions.
func createContextFileForOpencode(contextFile, commitMessage, sessionID string, prompts []string, summary string) error {
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
