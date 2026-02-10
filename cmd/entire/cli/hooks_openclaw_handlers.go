// hooks_openclaw_handlers.go contains OpenClaw specific hook handler implementations.
// These are called by the hook registry in hook_registry.go.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	openclawagent "github.com/entireio/cli/cmd/entire/cli/agent/openclaw"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

//nolint:gochecknoinits // Hook handler registration at startup is the intended pattern
func init() {
	// Register OpenClaw handlers
	RegisterHookHandler(agent.AgentNameOpenClaw, openclawagent.HookNameSaveSession, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleOpenClawSaveSession()
	})
}

// handleOpenClawSaveSession saves the current OpenClaw session transcript to
// the shadow branch so that the next git commit can create a checkpoint.
//
// This is the OpenClaw equivalent of Claude Code's "stop" hook - it takes the
// session transcript and persists it to Entire's metadata branch.
//
// Input (stdin JSON):
//
//	{
//	  "session_id": "...",
//	  "transcript_path": "/path/to/transcript.jsonl"
//	}
func handleOpenClawSaveSession() error {
	// Parse input
	ag, err := agent.Get(agent.AgentNameOpenClaw)
	if err != nil {
		return fmt.Errorf("failed to get openclaw agent: %w", err)
	}

	input, err := ag.ParseHookInput(agent.HookSessionEnd, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse input: %w", err)
	}

	sessionID := ag.GetSessionID(input)
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}

	transcriptPath := input.SessionRef
	if transcriptPath == "" {
		return fmt.Errorf("transcript_path is required")
	}

	// Read the transcript
	transcript, err := os.ReadFile(transcriptPath) //nolint:gosec // Path comes from OpenClaw
	if err != nil {
		return fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse transcript to extract modified files
	messages, err := openclawagent.ParseTranscript(transcript)
	if err != nil {
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	modifiedFiles := openclawagent.ExtractModifiedFiles(messages)

	// Get repo root for path normalization
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	// Normalize file paths to be repo-relative
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)

	if len(relModifiedFiles) == 0 {
		fmt.Fprintf(os.Stderr, "[entire] No file modifications found in transcript\n")
	} else {
		fmt.Fprintf(os.Stderr, "[entire] Files modified during session (%d):\n", len(relModifiedFiles))
		for _, file := range relModifiedFiles {
			fmt.Fprintf(os.Stderr, "  - %s\n", file)
		}
	}

	// Create session metadata directory
	sessionDir := filepath.Join(paths.EntireMetadataDir, sessionID)
	sessionDirAbs := filepath.Join(repoRoot, sessionDir)
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session dir: %w", err)
	}

	// Write transcript to metadata dir
	transcriptDest := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := os.WriteFile(transcriptDest, transcript, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	// Create minimal context file
	contextData := map[string]interface{}{
		"session_id": sessionID,
		"agent":      "openclaw",
		"files":      relModifiedFiles,
	}
	contextJSON, _ := json.MarshalIndent(contextData, "", "  ")
	contextFile := filepath.Join(sessionDirAbs, paths.ContextFileName)
	if err := os.WriteFile(contextFile, contextJSON, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to write context file: %v\n", err)
	}

	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get the strategy and save changes
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to ensure strategy setup: %v\n", err)
	}

	// Extract last user prompt for commit message
	lastPrompt := openclawagent.ExtractLastUserPrompt(messages)
	commitMsg := "OpenClaw session"
	if lastPrompt != "" {
		if len(lastPrompt) > 72 {
			commitMsg = lastPrompt[:72]
		} else {
			commitMsg = lastPrompt
		}
	}

	ctx := strategy.SaveContext{
		SessionID:      sessionID,
		ModifiedFiles:  relModifiedFiles,
		MetadataDir:    sessionDir,
		MetadataDirAbs: sessionDirAbs,
		CommitMessage:  commitMsg,
		TranscriptPath: transcriptPath,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
		AgentType:      agent.AgentTypeOpenClaw,
	}

	if err := strat.SaveChanges(ctx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[entire] Session saved to shadow branch\n")

	// Transition to turn end
	transitionSessionTurnEnd(sessionID)

	return nil
}
