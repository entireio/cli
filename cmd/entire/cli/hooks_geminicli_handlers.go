// hooks_geminicli_handlers.go contains Gemini CLI specific hook handler implementations.
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
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// ErrSessionSkipped is returned when a session should be skipped (e.g., due to concurrent warning).
var ErrSessionSkipped = errors.New("session skipped")

// handleGeminiSessionStart handles the SessionStart hook for Gemini CLI.
func handleGeminiSessionStart() error {
	return handleSessionStartCommon()
}

// handleGeminiSessionEnd handles the SessionEnd hook for Gemini CLI.
// This fires when the user explicitly exits the session (via "exit" or "logout" commands).
// It parses the session-end input and marks the session as ended so that subsequent
// git hooks (e.g., post-commit) can trigger condensation for ended sessions.
func handleGeminiSessionEnd() error { // Parse stdin once upfront — all subsequent steps use ctx.sessionID
	ctx, err := parseGeminiSessionEnd()
	if err != nil {
		if errors.Is(err, ErrSessionSkipped) {
			return nil
		}
		return fmt.Errorf("failed to parse session-end input: %w", err)
	}

	// Mark session as ended using the parsed session ID
	if err := markSessionEnded(ctx.sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark session ended: %v\n", err)
	}

	return nil
}

// geminiSessionContext holds parsed session data for Gemini commits.
type geminiSessionContext struct {
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

// parseGeminiSessionEnd parses the session-end hook input and validates transcript.
func parseGeminiSessionEnd() (*geminiSessionContext, error) {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookSessionEnd, os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "session-end",
		slog.String("hook", "session-end"),
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
		return nil, fmt.Errorf("transcript file not found or empty: %s", transcriptPath)
	}

	return &geminiSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
	}, nil
}

// setupGeminiSessionDir creates session directory and copies transcript.
func setupGeminiSessionDir(ctx *geminiSessionContext) error {
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

	preState, err := LoadPrePromptState(ctx.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}
	if preState != nil {
		fmt.Fprintf(os.Stderr, "Loaded pre-prompt state: %d pre-existing untracked files, start message index: %d\n", len(preState.UntrackedFiles), preState.StartMessageIndex)
	}

	// Get transcript position from pre-prompt state
	var startMessageIndex int
	if preState != nil {
		startMessageIndex = preState.StartMessageIndex
	}

	// Calculate token usage for this prompt/response cycle (Gemini-specific)
	var tokenUsage *agent.TokenUsage
	if ctx.transcriptPath != "" {
		usage, tokenErr := geminicli.CalculateTokenUsageFromFile(ctx.transcriptPath, startMessageIndex)
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
	if err := createContextFileForGemini(contextFile, ctx.commitMessage, ctx.sessionID, ctx.allPrompts, ctx.summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", ctx.sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()

	// Get agent type from the hook agent (determined by which hook command is running)
	// This is authoritative - if we're in "entire hooks gemini session-end", it's Gemini CLI
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
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
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
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPostToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
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

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())

	// Log with prompt if available (Gemini provides the user's prompt in BeforeAgent)
	logArgs := []any{
		slog.String("hook", "before-agent"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	}
	if input.UserPrompt != "" {
		// Truncate long prompts for logging
		const maxPromptPreviewLen = 100
		promptPreview := input.UserPrompt
		if len(promptPreview) > maxPromptPreviewLen {
			promptPreview = promptPreview[:maxPromptPreviewLen] + "..."
		}
		logArgs = append(logArgs, slog.String("prompt_preview", promptPreview))
	}
	logging.Info(logCtx, "gemini-before-agent", logArgs...)

	if input.SessionID == "" {
		return errors.New("no session_id in input")
	}

	// Capture pre-prompt state with transcript position (Gemini-specific)
	// This captures both untracked files and the current transcript message count
	// so we can calculate token usage for just this prompt/response cycle
	if err := CaptureGeminiPrePromptState(input.SessionID, input.SessionRef); err != nil {
		return fmt.Errorf("failed to capture pre-prompt state: %w", err)
	}

	// If strategy implements SessionInitializer, call it to initialize session state
	strat := GetStrategy()

	// Ensure strategy setup is in place (git hooks, gitignore, metadata branch).
	// Done here at turn start so hooks are installed before any mid-turn commits.
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		agentType := ag.Type()
		if err := initializer.InitializeSession(input.SessionID, agentType, input.SessionRef, input.UserPrompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
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

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "gemini-after-agent",
		slog.String("hook", "after-agent"),
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

	// Early check: bail out quickly if the repo has no commits yet.
	if repo, err := strategy.OpenRepository(); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(os.Stderr, "Entire: skipping checkpoint. Will activate after first commit.")
		return NewSilentError(strategy.ErrEmptyRepository)
	}

	// Create session context and commit
	ctx := &geminiSessionContext{
		sessionID:      sessionID,
		transcriptPath: transcriptPath,
	}

	if err := setupGeminiSessionDir(ctx); err != nil {
		return err
	}

	if err := extractGeminiMetadata(ctx); err != nil {
		return err
	}

	if err := commitGeminiSession(ctx); err != nil {
		return err
	}

	// Transition session ACTIVE → IDLE (equivalent to Claude's transitionSessionTurnEnd)
	transitionSessionTurnEnd(sessionID)

	return nil
}

// handleGeminiBeforeModel handles the BeforeModel hook for Gemini CLI.
// This fires before every LLM call (potentially multiple times per agent loop).
// Useful for logging/monitoring LLM requests.
func handleGeminiBeforeModel() error {
	// Get the agent for hook input parsing
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input - use HookPreToolUse as a generic hook type for now
	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
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
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPostToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
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
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
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
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookSessionStart, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
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
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Parse hook input
	input, err := ag.ParseHookInput(agent.HookSessionStart, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Debug(logCtx, "gemini-notification",
		slog.String("hook", "notification"),
		slog.String("hook_type", "notification"),
		slog.String("model_session_id", input.SessionID),
	)

	// For now, Notification is mainly for logging
	// Future: Could be used for error tracking/alerting
	return nil
}
