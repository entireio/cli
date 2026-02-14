// hooks_common_session.go contains shared session commit logic used by multiple agent handlers.
// This avoids duplicating the commit pipeline across Codex, OpenCode, Gemini, and future agents.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// agentCommitContext holds all data needed to commit an agent session.
// Each agent handler populates this struct before calling commitAgentSession.
type agentCommitContext struct {
	sessionID      string
	sessionDir     string
	sessionDirAbs  string
	commitMessage  string
	transcriptPath string

	// File sources — agents provide one or both approaches.
	// transcriptModifiedFiles: files extracted from agent transcript (Codex, Gemini).
	// useGitStatusForModified: if true, use git status for modified files instead of transcript (OpenCode).
	transcriptModifiedFiles []string
	useGitStatusForModified bool

	// Context file content
	prompts []string // Single prompt → len==1; Gemini multi-prompt → len>1
	summary string   // Gemini only

	// Optional per-step enrichment (Gemini-specific)
	stepTranscriptStart      int               // Message offset for token calculation
	stepTranscriptIdentifier string            // Last message ID at step start
	tokenUsage               *agent.TokenUsage // Calculated token stats
}

// singlePrompt wraps a single prompt string into a slice for agentCommitContext.prompts.
// Returns nil if the prompt is empty.
func singlePrompt(prompt string) []string {
	if prompt == "" {
		return nil
	}
	return []string{prompt}
}

// commitAgentSession is the shared commit pipeline for non-Claude-Code agents.
// It handles: load pre-prompt state → detect file changes → filter/normalize paths →
// skip if no changes → log changes → create context file → get author → save via strategy.
func commitAgentSession(ctx *agentCommitContext) error {
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
	changes, err := DetectFileChanges(preState.PreUntrackedFiles())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to compute file changes: %v\n", err)
	}

	// Determine modified files: either from transcript extraction or git status
	var relModifiedFiles []string
	if ctx.useGitStatusForModified {
		if changes != nil {
			relModifiedFiles = FilterAndNormalizePaths(changes.Modified, repoRoot)
		}
	} else {
		relModifiedFiles = FilterAndNormalizePaths(ctx.transcriptModifiedFiles, repoRoot)
	}

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
	if err := createContextFile(contextFile, ctx.commitMessage, ctx.sessionID, ctx.prompts, ctx.summary); err != nil {
		return fmt.Errorf("failed to create context file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created context file: %s\n", ctx.sessionDir+"/"+paths.ContextFileName)

	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	strat := GetStrategy()

	// Get agent type from the hook agent
	hookAgent, agentErr := GetCurrentHookAgent()
	if agentErr != nil {
		return fmt.Errorf("failed to get agent: %w", agentErr)
	}
	agentType := hookAgent.Type()

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
		StepTranscriptStart:      ctx.stepTranscriptStart,
		StepTranscriptIdentifier: ctx.stepTranscriptIdentifier,
		TokenUsage:               ctx.tokenUsage,
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

// createContextFile creates a context.md file for agent sessions.
// Supports single prompt (len==1) or multi-prompt (Gemini) and optional summary.
func createContextFile(contextFile, commitMessage, sessionID string, prompts []string, summary string) error {
	var sb strings.Builder

	sb.WriteString("# Session Context\n\n")
	sb.WriteString(fmt.Sprintf("Session ID: %s\n", sessionID))
	sb.WriteString(fmt.Sprintf("Commit Message: %s\n\n", commitMessage))

	if len(prompts) == 1 {
		sb.WriteString("## Prompt\n\n")
		sb.WriteString(prompts[0])
		sb.WriteString("\n")
	} else if len(prompts) > 1 {
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

// transitionSessionTurnEnd fires EventTurnEnd to move the session from
// ACTIVE → IDLE (or ACTIVE_COMMITTED → IDLE). Best-effort: logs warnings
// on failure rather than returning errors.
func transitionSessionTurnEnd(sessionID string) {
	turnState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state for turn end: %v\n", loadErr)
		return
	}
	if turnState == nil {
		return
	}
	remaining := strategy.TransitionAndLog(turnState, session.EventTurnEnd, session.TransitionContext{})

	// Dispatch strategy-specific actions (e.g., ActionCondense for ACTIVE_COMMITTED → IDLE)
	if len(remaining) > 0 {
		strat := GetStrategy()
		if handler, ok := strat.(strategy.TurnEndHandler); ok {
			if err := handler.HandleTurnEnd(turnState, remaining); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: turn-end action dispatch failed: %v\n", err)
			}
		}
	}

	if updateErr := strategy.SaveSessionState(turnState); updateErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update session phase on turn end: %v\n", updateErr)
	}
}
