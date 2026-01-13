package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cpkg "entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/textutil"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// listCheckpoints returns all checkpoints from the sessions branch.
// Uses checkpoint.GitStore.ListCommitted() for reading from entire/sessions.
func (s *ManualCommitStrategy) listCheckpoints() ([]CheckpointInfo, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	committed, err := store.ListCommitted(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to list committed checkpoints: %w", err)
	}

	// Convert from checkpoint.CommittedInfo to strategy.CheckpointInfo
	result := make([]CheckpointInfo, 0, len(committed))
	for _, c := range committed {
		result = append(result, CheckpointInfo{
			CheckpointID:     c.CheckpointID,
			SessionID:        c.SessionID,
			CreatedAt:        c.CreatedAt,
			CheckpointsCount: c.CheckpointsCount,
			FilesTouched:     c.FilesTouched,
			Agent:            c.Agent,
			IsTask:           c.IsTask,
			ToolUseID:        c.ToolUseID,
			SessionCount:     c.SessionCount,
			SessionIDs:       c.SessionIDs,
		})
	}

	return result, nil
}

// getCheckpointsForSession returns all checkpoints for a session ID.
func (s *ManualCommitStrategy) getCheckpointsForSession(sessionID string) ([]CheckpointInfo, error) {
	all, err := s.listCheckpoints()
	if err != nil {
		return nil, err
	}

	var result []CheckpointInfo
	for _, cp := range all {
		if cp.SessionID == sessionID || strings.HasPrefix(cp.SessionID, sessionID) {
			result = append(result, cp)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no checkpoints for session: %s", sessionID)
	}

	return result, nil
}

// getCheckpointLog returns the transcript for a specific checkpoint ID.
// Uses checkpoint.GitStore.ReadCommitted() for reading from entire/sessions.
func (s *ManualCommitStrategy) getCheckpointLog(checkpointID string) ([]byte, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	result, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("checkpoint not found: %s", checkpointID)
	}
	if len(result.Transcript) == 0 {
		return nil, fmt.Errorf("no transcript found for checkpoint: %s", checkpointID)
	}

	return result.Transcript, nil
}

// CondenseSession condenses a session's shadow branch to permanent storage.
// checkpointID is the 12-hex-char value from the Entire-Checkpoint trailer.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
// Uses checkpoint.GitStore.WriteCommitted for the git operations.
func (s *ManualCommitStrategy) CondenseSession(repo *git.Repository, checkpointID string, state *SessionState) (*CondenseResult, error) {
	// Get shadow branch
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("shadow branch not found: %w", err)
	}

	// Extract session data, starting from where we left off last condensation
	// Use tracked files from session state instead of collecting all files from tree
	// Pass agent type to handle different transcript formats (JSONL for Claude, JSON for Gemini)
	sessionData, err := s.extractSessionData(repo, ref.Hash(), state.SessionID, state.CondensedTranscriptLines, state.FilesTouched, state.AgentType)
	if err != nil {
		return nil, fmt.Errorf("failed to extract session data: %w", err)
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Get author info
	authorName, authorEmail := GetGitAuthorFromRepo(repo)

	// Write checkpoint metadata using the checkpoint store
	if err := store.WriteCommitted(context.Background(), cpkg.WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        state.SessionID,
		Strategy:         StrategyNameManualCommit,
		Transcript:       sessionData.Transcript,
		Prompts:          sessionData.Prompts,
		Context:          sessionData.Context,
		FilesTouched:     sessionData.FilesTouched,
		CheckpointsCount: state.CheckpointCount,
		EphemeralBranch:  shadowBranchName,
		AuthorName:       authorName,
		AuthorEmail:      authorEmail,
		Agent:            state.AgentType,
	}); err != nil {
		return nil, fmt.Errorf("failed to write checkpoint metadata: %w", err)
	}

	return &CondenseResult{
		CheckpointID:         checkpointID,
		SessionID:            state.SessionID,
		CheckpointsCount:     state.CheckpointCount,
		FilesTouched:         sessionData.FilesTouched,
		TotalTranscriptLines: sessionData.FullTranscriptLines,
	}, nil
}

// extractSessionData extracts session data from the shadow branch.
// startLine specifies the first line to include (0 = all lines, for incremental condensation).
// filesTouched is the list of files tracked during the session (from SessionState.FilesTouched).
// agentType identifies the agent (e.g., "Gemini CLI", "Claude Code") to determine transcript format.
func (s *ManualCommitStrategy) extractSessionData(repo *git.Repository, shadowRef plumbing.Hash, sessionID string, startLine int, filesTouched []string, agentType string) (*ExtractedSessionData, error) {
	commit, err := repo.CommitObject(shadowRef)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	data := &ExtractedSessionData{}
	// sessionID is already an "entire session ID" (with date prefix), so construct path directly
	// Don't use paths.SessionMetadataDir which would add another date prefix
	metadataDir := paths.EntireMetadataDir + "/" + sessionID

	// Extract transcript
	var fullTranscript string
	if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			fullTranscript = content
		}
	} else if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileNameLegacy); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			fullTranscript = content
		}
	}

	// Process transcript based on agent type
	if fullTranscript != "" {
		// Check if this is a Gemini CLI transcript (JSON format, not JSONL)
		isGeminiFormat := strings.Contains(agentType, "Gemini") || isGeminiJSONTranscript(fullTranscript)

		if isGeminiFormat {
			// Gemini uses JSON format with a "messages" array
			data.Transcript = []byte(fullTranscript)
			data.FullTranscriptLines = 1 // JSON is a single "line"
			data.Prompts = extractUserPromptsFromGeminiJSON(fullTranscript)
			data.Context = generateContextFromPrompts(data.Prompts)
		} else {
			// Claude Code and others use JSONL format (one JSON object per line)
			allLines := strings.Split(fullTranscript, "\n")

			// Trim trailing empty lines (from final \n in JSONL)
			for len(allLines) > 0 && strings.TrimSpace(allLines[len(allLines)-1]) == "" {
				allLines = allLines[:len(allLines)-1]
			}

			data.FullTranscriptLines = len(allLines)

			// Get only lines from startLine onwards for this condensation
			if startLine < len(allLines) {
				newLines := allLines[startLine:]
				data.Transcript = []byte(strings.Join(newLines, "\n"))

				// Extract prompts from the new portion only
				data.Prompts = extractUserPromptsFromLines(newLines)

				// Generate context from prompts
				data.Context = generateContextFromPrompts(data.Prompts)
			}
		}
	}

	// Use tracked files from session state (not all files in tree)
	data.FilesTouched = filesTouched

	return data, nil
}

// isGeminiJSONTranscript detects if the transcript is in Gemini's JSON format.
// Gemini transcripts start with a JSON object containing a "messages" array.
func isGeminiJSONTranscript(content string) bool {
	content = strings.TrimSpace(content)
	// Quick check: Gemini JSON starts with { and contains "messages"
	if !strings.HasPrefix(content, "{") {
		return false
	}
	// Try to parse as Gemini format
	var transcript struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(content), &transcript); err != nil {
		return false
	}
	return len(transcript.Messages) > 0
}

// extractUserPromptsFromGeminiJSON extracts user prompts from Gemini's JSON transcript format.
// Gemini transcripts are structured as: {"messages": [{"type": "user", "content": "..."}, ...]}
func extractUserPromptsFromGeminiJSON(content string) []string {
	var transcript struct {
		Messages []struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal([]byte(content), &transcript); err != nil {
		return nil
	}

	var prompts []string
	for _, msg := range transcript.Messages {
		if msg.Type == "user" && msg.Content != "" {
			// Strip IDE context tags for consistency with Claude Code handling
			cleaned := textutil.StripIDEContextTags(msg.Content)
			if cleaned != "" {
				prompts = append(prompts, cleaned)
			}
		}
	}

	return prompts
}

// extractUserPromptsFromLines extracts user prompts from JSONL transcript lines.
// IDE-injected context tags (like <ide_opened_file>) are stripped from the results.
func extractUserPromptsFromLines(lines []string) []string {
	var prompts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		// Check for user message (supports both "human" and "user" types)
		msgType, ok := entry["type"].(string)
		if !ok || (msgType != "human" && msgType != "user") {
			continue
		}

		// Extract message content
		message, ok := entry["message"].(map[string]interface{})
		if !ok {
			continue
		}

		// Handle string content
		if content, ok := message["content"].(string); ok && content != "" {
			cleaned := textutil.StripIDEContextTags(content)
			if cleaned != "" {
				prompts = append(prompts, cleaned)
			}
			continue
		}

		// Handle array content (e.g., multiple text blocks from VSCode)
		if arr, ok := message["content"].([]interface{}); ok {
			var texts []string
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if m["type"] == "text" {
						if text, ok := m["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
			}
			if len(texts) > 0 {
				cleaned := textutil.StripIDEContextTags(strings.Join(texts, "\n\n"))
				if cleaned != "" {
					prompts = append(prompts, cleaned)
				}
			}
		}
	}
	return prompts
}

// generateContextFromPrompts generates context.md content from a list of prompts.
func generateContextFromPrompts(prompts []string) []byte {
	if len(prompts) == 0 {
		return nil
	}

	var buf strings.Builder
	buf.WriteString("# Session Context\n\n")
	buf.WriteString("## User Prompts\n\n")

	for i, prompt := range prompts {
		// Truncate very long prompts for readability
		displayPrompt := prompt
		if len(displayPrompt) > 500 {
			displayPrompt = displayPrompt[:500] + "..."
		}
		buf.WriteString(fmt.Sprintf("### Prompt %d\n\n", i+1))
		buf.WriteString(displayPrompt)
		buf.WriteString("\n\n")
	}

	return []byte(buf.String())
}
