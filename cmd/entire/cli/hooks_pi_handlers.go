// hooks_pi_handlers.go contains Pi-specific hook handler implementations.
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
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// handlePiSessionStart handles the SessionStart hook for Pi.
func handlePiSessionStart() error {
	return handleSessionStartCommon()
}

// handlePiUserPromptSubmit handles Pi's user-prompt-submit hook.
func handlePiUserPromptSubmit() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookUserPromptSubmit, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "user-prompt-submit",
		slog.String("hook", "user-prompt-submit"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
		slog.String("transcript_path", input.SessionRef),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	if err := CapturePrePromptState(sessionID, input.SessionRef); err != nil {
		return err
	}

	strat := GetStrategy()
	if initializer, ok := strat.(strategy.SessionInitializer); ok {
		if err := initializer.InitializeSession(sessionID, ag.Type(), input.SessionRef, input.UserPrompt); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize session state: %v\n", err)
		}
	}

	return nil
}

// handlePiBeforeTool handles Pi's before-tool hook (mapped from Pi tool_call event).
func handlePiBeforeTool() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookPreToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Debug(logCtx, "pi-before-tool",
		slog.String("hook", "before-tool"),
		slog.String("hook_type", "tool"),
		slog.String("model_session_id", input.SessionID),
		slog.String("tool_name", input.ToolName),
	)

	return nil
}

// handlePiAfterTool handles Pi's after-tool hook (mapped from Pi tool_result event).
func handlePiAfterTool() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookPostToolUse, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Debug(logCtx, "pi-after-tool",
		slog.String("hook", "after-tool"),
		slog.String("hook_type", "tool"),
		slog.String("model_session_id", input.SessionID),
		slog.String("tool_name", input.ToolName),
	)

	return nil
}

// handlePiStop handles Pi's stop hook (mapped from Pi's agent_end lifecycle event).
func handlePiStop() error { //nolint:maintidx // mirrors existing stop handler complexity
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookStop, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "stop",
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

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := copyFile(transcriptPath, logFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Copied transcript to: %s\n", sessionDir+"/"+paths.TranscriptFileName)

	preState, err := LoadPrePromptState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load pre-prompt state: %v\n", err)
	}

	transcriptOffset := resolvePiTranscriptOffset(sessionID, preState)
	if transcriptOffset > 0 {
		fmt.Fprintf(os.Stderr, "Parsing Pi transcript from line %d\n", transcriptOffset)
	}

	leafID := hookInputRawString(input, "leaf_id")
	entries, totalLines, err := pi.ParseTranscriptFromLineWithLeaf(transcriptPath, transcriptOffset, leafID)
	if err != nil {
		return fmt.Errorf("failed to parse Pi transcript from line %d: %w", transcriptOffset, err)
	}

	allPrompts := pi.ExtractAllUserPromptsFromEntries(entries)
	promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)
	promptContent := strings.Join(allPrompts, "\n\n---\n\n")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted %d prompt(s) to: %s\n", len(allPrompts), sessionDir+"/"+paths.PromptFileName)

	summary := pi.ExtractLastAssistantMessageFromEntries(entries)
	summaryFile := filepath.Join(sessionDirAbs, paths.SummaryFileName)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Extracted summary to: %s\n", sessionDir+"/"+paths.SummaryFileName)

	modifiedFiles := hookInputRawStringSlice(input, "modified_files")
	if len(modifiedFiles) == 0 {
		modifiedFiles = pi.ExtractModifiedFilesFromEntries(entries)
	}

	lastPrompt := ""
	if len(allPrompts) > 0 {
		lastPrompt = allPrompts[len(allPrompts)-1]
	}
	commitMessage := generateCommitMessage(lastPrompt)
	fmt.Fprintf(os.Stderr, "Using commit message: %s\n", commitMessage)

	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	if preState != nil {
		fmt.Fprintf(os.Stderr, "Pre-prompt state: %d pre-existing untracked files\n", len(preState.UntrackedFiles))
	}

	var preUntracked []string
	if preState != nil {
		preUntracked = preState.PreUntrackedFiles()
	}

	changes, err := DetectFileChanges(preUntracked)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles []string
	var relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintln(os.Stderr, "No files were modified during this session")
		fmt.Fprintln(os.Stderr, "Skipping commit")
		persistPiTranscriptLeaf(sessionID, leafID)
		transitionSessionTurnEnd(sessionID)
		if err := CleanupPrePromptState(sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
		}
		return nil
	}

	logFileChanges(relModifiedFiles, relNewFiles, relDeletedFiles)

	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := createContextFileForPi(contextFile, commitMessage, sessionID, allPrompts, summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.StepTranscriptStart
	}

	tokenUsage := pi.CalculateTokenUsageFromEntries(entries)

	saveCtx := strategy.SaveContext{
		SessionID:                sessionID,
		ModifiedFiles:            relModifiedFiles,
		NewFiles:                 relNewFiles,
		DeletedFiles:             relDeletedFiles,
		MetadataDir:              sessionDir,
		MetadataDirAbs:           sessionDirAbs,
		CommitMessage:            commitMessage,
		TranscriptPath:           transcriptPath,
		AuthorName:               author.Name,
		AuthorEmail:              author.Email,
		AgentType:                ag.Type(),
		StepTranscriptStart:      transcriptLinesAtStart,
		StepTranscriptIdentifier: transcriptIdentifierAtStart,
		TranscriptLeafID:         leafID,
		TokenUsage:               tokenUsage,
	}

	if err := strat.SaveChanges(saveCtx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	if strat.Name() == strategy.StrategyNameAutoCommit {
		sessionState, loadErr := strategy.LoadSessionState(sessionID)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", loadErr)
		}
		if sessionState == nil {
			sessionState = &strategy.SessionState{SessionID: sessionID}
		}
		sessionState.CheckpointTranscriptStart = totalLines
		sessionState.StepCount++
		if leafID != "" {
			sessionState.TranscriptLeafID = leafID
		}
		if updateErr := strategy.SaveSessionState(sessionState); updateErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update session state: %v\n", updateErr)
		} else {
			fmt.Fprintf(os.Stderr, "Updated session state: transcript position=%d, checkpoint=%d\n", totalLines, sessionState.StepCount)
		}
	}

	transitionSessionTurnEnd(sessionID)

	if err := CleanupPrePromptState(sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup pre-prompt state: %v\n", err)
	}

	return nil
}

func hookInputRawString(input *agent.HookInput, key string) string {
	if input == nil || input.RawData == nil {
		return ""
	}
	value, ok := input.RawData[key]
	if !ok {
		return ""
	}
	str, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(str)
}

func hookInputRawStringSlice(input *agent.HookInput, key string) []string {
	if input == nil || input.RawData == nil {
		return nil
	}
	value, ok := input.RawData[key]
	if !ok {
		return nil
	}

	switch typed := value.(type) {
	case []string:
		if len(typed) == 0 {
			return nil
		}
		copied := make([]string, 0, len(typed))
		for _, item := range typed {
			trimmed := strings.TrimSpace(item)
			if trimmed != "" {
				copied = append(copied, trimmed)
			}
		}
		return copied
	case []interface{}:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			str, ok := item.(string)
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(str)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
		if len(result) == 0 {
			return nil
		}
		return result
	default:
		return nil
	}
}

func persistPiTranscriptLeaf(sessionID, leafID string) {
	leafID = strings.TrimSpace(leafID)
	if leafID == "" {
		return
	}

	state, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state for leaf persistence: %v\n", err)
		return
	}
	if state == nil {
		return
	}

	if state.TranscriptLeafID == leafID {
		return
	}
	state.TranscriptLeafID = leafID
	if err := strategy.SaveSessionState(state); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to persist transcript leaf id: %v\n", err)
	}
}

func resolvePiTranscriptOffset(sessionID string, preState *PrePromptState) int {
	if preState != nil && preState.StepTranscriptStart > 0 {
		return preState.StepTranscriptStart
	}

	sessionState, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", err)
		return 0
	}
	if sessionState != nil && sessionState.CheckpointTranscriptStart > 0 {
		return sessionState.CheckpointTranscriptStart
	}

	return 0
}

// createContextFileForPi creates a context.md file for Pi sessions.
func createContextFileForPi(contextFile, commitMessage, sessionID string, prompts []string, summary string) error {
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

// handlePiSessionEnd handles Pi's session-end hook.
func handlePiSessionEnd() error {
	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookSessionEnd, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse hook input: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Info(logCtx, "session-end",
		slog.String("hook", "session-end"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", input.SessionID),
	)

	if input.SessionID == "" {
		return nil
	}

	if err := markSessionEnded(input.SessionID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark session ended: %v\n", err)
	}

	return nil
}
