package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// listCheckpoints returns all checkpoints from the metadata branch.
// Uses checkpoint.GitStore.ListCommitted() for reading from entire/checkpoints/v1.
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
// Uses checkpoint.GitStore.ReadCommitted() for reading from entire/checkpoints/v1.
func (s *ManualCommitStrategy) getCheckpointLog(checkpointID id.CheckpointID) ([]byte, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	content, err := store.ReadLatestSessionContent(context.Background(), checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if content == nil {
		return nil, fmt.Errorf("checkpoint not found: %s", checkpointID)
	}
	if len(content.Transcript) == 0 {
		return nil, fmt.Errorf("no transcript found for checkpoint: %s", checkpointID)
	}

	return content.Transcript, nil
}

// CondenseSession condenses a session's shadow branch to permanent storage.
// checkpointID is the 12-hex-char value from the Entire-Checkpoint trailer.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
// Uses checkpoint.GitStore.WriteCommitted for the git operations.
func (s *ManualCommitStrategy) CondenseSession(repo *git.Repository, checkpointID id.CheckpointID, state *SessionState) (*CondenseResult, error) {
	// Get shadow branch
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("shadow branch not found: %w", err)
	}

	// Extract session data from the shadow branch (with live transcript fallback).
	// Use tracked files from session state instead of collecting all files from tree.
	// Pass agent type to handle different transcript formats (JSONL for Claude, JSON for Gemini).
	// Pass live transcript path so condensation reads the current file rather than a
	// potentially stale shadow branch copy (SaveChanges may have been skipped if the
	// last turn had no code changes).
	// Pass CheckpointTranscriptStart for accurate token calculation (line offset for Claude, message index for Gemini).
	sessionData, err := s.extractSessionData(repo, ref.Hash(), state.SessionID, state.FilesTouched, state.AgentType, state.TranscriptPath, state.CheckpointTranscriptStart)
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
	attribution := calculateSessionAttributions(repo, ref, sessionData, state)
	// Get current branch name
	branchName := GetCurrentBranchName(repo)

	// Generate summary if enabled
	var summary *cpkg.Summary
	if settings.IsSummarizeEnabled() && len(sessionData.Transcript) > 0 {
		logCtx := logging.WithComponent(context.Background(), "attribution")
		summarizeCtx := logging.WithComponent(logCtx, "summarize")

		// Scope transcript to this checkpoint's portion.
		// SliceFromLine only works for JSONL (Claude Code) transcripts where
		// CheckpointTranscriptStart is a line offset. For Gemini JSON transcripts,
		// CheckpointTranscriptStart is a message index and the transcript is a single
		// JSON blob, so line-based slicing would produce malformed content.
		// The summarizer (ParseFromBytes) only supports JSONL, so skip scoping for Gemini.
		var scopedTranscript []byte
		if state.AgentType == agent.AgentTypeGemini {
			scopedTranscript = sessionData.Transcript
		} else {
			scopedTranscript = transcript.SliceFromLine(sessionData.Transcript, state.CheckpointTranscriptStart)
		}
		if len(scopedTranscript) > 0 {
			var err error
			summary, err = summarize.GenerateFromTranscript(summarizeCtx, scopedTranscript, sessionData.FilesTouched, nil)
			if err != nil {
				logging.Warn(summarizeCtx, "summary generation failed",
					slog.String("session_id", state.SessionID),
					slog.String("error", err.Error()))
				// Continue without summary - non-blocking
			} else {
				logging.Info(summarizeCtx, "summary generated",
					slog.String("session_id", state.SessionID))
			}
		}
	}

	// Write checkpoint metadata using the checkpoint store
	if err := store.WriteCommitted(context.Background(), cpkg.WriteCommittedOptions{
		CheckpointID:                checkpointID,
		SessionID:                   state.SessionID,
		Strategy:                    StrategyNameManualCommit,
		Branch:                      branchName,
		Transcript:                  sessionData.Transcript,
		Prompts:                     sessionData.Prompts,
		Context:                     sessionData.Context,
		FilesTouched:                sessionData.FilesTouched,
		CheckpointsCount:            state.StepCount,
		EphemeralBranch:             shadowBranchName,
		AuthorName:                  authorName,
		AuthorEmail:                 authorEmail,
		Agent:                       state.AgentType,
		TranscriptIdentifierAtStart: state.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   state.CheckpointTranscriptStart,
		TokenUsage:                  sessionData.TokenUsage,
		InitialAttribution:          attribution,
		Summary:                     summary,
		SessionTranscriptPath:       homeRelativePath(state.TranscriptPath),
	}); err != nil {
		return nil, fmt.Errorf("failed to write checkpoint metadata: %w", err)
	}

	return &CondenseResult{
		CheckpointID:         checkpointID,
		SessionID:            state.SessionID,
		CheckpointsCount:     state.StepCount,
		FilesTouched:         sessionData.FilesTouched,
		TotalTranscriptLines: sessionData.FullTranscriptLines,
	}, nil
}

func calculateSessionAttributions(repo *git.Repository, shadowRef *plumbing.Reference, sessionData *ExtractedSessionData, state *SessionState) *cpkg.InitialAttribution {
	// Calculate initial attribution using accumulated prompt attribution data.
	// This uses user edits captured at each prompt start (before agent works),
	// plus any user edits after the final checkpoint (shadow → head).
	logCtx := logging.WithComponent(context.Background(), "attribution")
	var attribution *cpkg.InitialAttribution
	headRef, headErr := repo.Head()
	if headErr != nil {
		logging.Debug(logCtx, "attribution skipped: failed to get HEAD",
			slog.String("error", headErr.Error()))
	} else {
		headCommit, commitErr := repo.CommitObject(headRef.Hash())
		if commitErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD commit",
				slog.String("error", commitErr.Error()))
		} else {
			headTree, treeErr := headCommit.Tree()
			if treeErr != nil {
				logging.Debug(logCtx, "attribution skipped: failed to get HEAD tree",
					slog.String("error", treeErr.Error()))
			} else {
				// Get shadow branch tree (checkpoint tree - what the agent wrote)
				shadowCommit, shadowErr := repo.CommitObject(shadowRef.Hash())
				if shadowErr != nil {
					logging.Debug(logCtx, "attribution skipped: failed to get shadow commit",
						slog.String("error", shadowErr.Error()),
						slog.String("shadow_ref", shadowRef.Hash().String()))
				} else {
					shadowTree, shadowTreeErr := shadowCommit.Tree()
					if shadowTreeErr != nil {
						logging.Debug(logCtx, "attribution skipped: failed to get shadow tree",
							slog.String("error", shadowTreeErr.Error()))
					} else {
						// Get base tree (state before session started)
						var baseTree *object.Tree
						attrBase := state.AttributionBaseCommit
						if attrBase == "" {
							attrBase = state.BaseCommit // backward compat
						}
						if baseCommit, baseErr := repo.CommitObject(plumbing.NewHash(attrBase)); baseErr == nil {
							if tree, baseTErr := baseCommit.Tree(); baseTErr == nil {
								baseTree = tree
							} else {
								logging.Debug(logCtx, "attribution: base tree unavailable",
									slog.String("error", baseTErr.Error()))
							}
						} else {
							logging.Debug(logCtx, "attribution: base commit unavailable",
								slog.String("error", baseErr.Error()),
								slog.String("attribution_base", attrBase))
						}

						// Log accumulated prompt attributions for debugging
						var totalUserAdded, totalUserRemoved int
						for i, pa := range state.PromptAttributions {
							totalUserAdded += pa.UserLinesAdded
							totalUserRemoved += pa.UserLinesRemoved
							logging.Debug(logCtx, "prompt attribution data",
								slog.Int("checkpoint", pa.CheckpointNumber),
								slog.Int("user_added", pa.UserLinesAdded),
								slog.Int("user_removed", pa.UserLinesRemoved),
								slog.Int("agent_added", pa.AgentLinesAdded),
								slog.Int("agent_removed", pa.AgentLinesRemoved),
								slog.Int("index", i))
						}

						attribution = CalculateAttributionWithAccumulated(
							baseTree,
							shadowTree,
							headTree,
							sessionData.FilesTouched,
							state.PromptAttributions,
						)

						if attribution != nil {
							logging.Info(logCtx, "attribution calculated",
								slog.Int("agent_lines", attribution.AgentLines),
								slog.Int("human_added", attribution.HumanAdded),
								slog.Int("human_modified", attribution.HumanModified),
								slog.Int("human_removed", attribution.HumanRemoved),
								slog.Int("total_committed", attribution.TotalCommitted),
								slog.Float64("agent_percentage", attribution.AgentPercentage),
								slog.Int("accumulated_user_added", totalUserAdded),
								slog.Int("accumulated_user_removed", totalUserRemoved),
								slog.Int("files_touched", len(sessionData.FilesTouched)))
						}
					}
				}
			}
		}
	}
	return attribution
}

// extractSessionData extracts session data from the shadow branch.
// filesTouched is the list of files tracked during the session (from SessionState.FilesTouched).
// agentType identifies the agent (e.g., "Gemini CLI", "Claude Code") to determine transcript format.
// liveTranscriptPath, when non-empty and readable, is preferred over the shadow branch copy.
// This handles the case where SaveChanges was skipped (no code changes) but the transcript
// continued growing — the shadow branch copy would be stale.
// checkpointTranscriptStart is the line offset (Claude) or message index (Gemini) where the current checkpoint began.
func (s *ManualCommitStrategy) extractSessionData(repo *git.Repository, shadowRef plumbing.Hash, sessionID string, filesTouched []string, agentType agent.AgentType, liveTranscriptPath string, checkpointTranscriptStart int) (*ExtractedSessionData, error) {
	commit, err := repo.CommitObject(shadowRef)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	data := &ExtractedSessionData{}
	// sessionID is already an "entire session ID" (with date prefix)
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)

	// Extract transcript — prefer the live file when available, fall back to shadow branch.
	// The shadow branch copy may be stale if the last turn ended without code changes
	// (SaveChanges is only called when there are file modifications).
	var fullTranscript string
	if liveTranscriptPath != "" {
		if liveData, readErr := os.ReadFile(liveTranscriptPath); readErr == nil && len(liveData) > 0 { //nolint:gosec // path from session state
			fullTranscript = string(liveData)
		}
	}
	if fullTranscript == "" {
		// Fall back to shadow branch copy
		if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileName); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil {
				fullTranscript = content
			}
		} else if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileNameLegacy); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil {
				fullTranscript = content
			}
		}
	}

	// Process transcript based on agent type
	if fullTranscript != "" {
		data.Transcript = []byte(fullTranscript)
		data.FullTranscriptLines = countTranscriptItems(agentType, fullTranscript)
		data.Prompts = extractUserPrompts(agentType, fullTranscript)
		data.Context = generateContextFromPrompts(data.Prompts)
	}

	// Use tracked files from session state (not all files in tree)
	data.FilesTouched = filesTouched

	// Calculate token usage from the extracted transcript portion
	if len(data.Transcript) > 0 {
		data.TokenUsage = calculateTokenUsage(agentType, data.Transcript, checkpointTranscriptStart)
	}

	return data, nil
}

// countTranscriptItems counts lines (JSONL) or messages (JSON) in a transcript.
// For Claude Code and JSONL-based agents, this counts lines.
// For Gemini CLI and JSON-based agents, this counts messages.
// Returns 0 if the content is empty or malformed.
func countTranscriptItems(agentType agent.AgentType, content string) int {
	if content == "" {
		return 0
	}

	// Try Gemini format first if agentType is Gemini, or as fallback if Unknown
	if agentType == agent.AgentTypeGemini || agentType == agent.AgentTypeUnknown {
		transcript, err := geminicli.ParseTranscript([]byte(content))
		if err == nil && transcript != nil && len(transcript.Messages) > 0 {
			return len(transcript.Messages)
		}
		// If agentType is explicitly Gemini but parsing failed, return 0
		if agentType == agent.AgentTypeGemini {
			return 0
		}
		// Otherwise fall through to JSONL parsing for Unknown type
	}

	// Claude Code and other JSONL-based agents
	allLines := strings.Split(content, "\n")
	// Trim trailing empty lines (from final \n in JSONL)
	for len(allLines) > 0 && strings.TrimSpace(allLines[len(allLines)-1]) == "" {
		allLines = allLines[:len(allLines)-1]
	}
	return len(allLines)
}

// extractUserPrompts extracts all user prompts from transcript content.
// Returns prompts with IDE context tags stripped (e.g., <ide_opened_file>).
func extractUserPrompts(agentType agent.AgentType, content string) []string {
	if content == "" {
		return nil
	}

	// Try Gemini format first if agentType is Gemini, or as fallback if Unknown
	if agentType == agent.AgentTypeGemini || agentType == agent.AgentTypeUnknown {
		prompts, err := geminicli.ExtractAllUserPrompts([]byte(content))
		if err == nil && len(prompts) > 0 {
			// Strip IDE context tags for consistency with Claude Code handling
			cleaned := make([]string, 0, len(prompts))
			for _, prompt := range prompts {
				if stripped := textutil.StripIDEContextTags(prompt); stripped != "" {
					cleaned = append(cleaned, stripped)
				}
			}
			return cleaned
		}
		// If agentType is explicitly Gemini but parsing failed, return nil
		if agentType == agent.AgentTypeGemini {
			return nil
		}
		// Otherwise fall through to JSONL parsing for Unknown type
	}

	// Claude Code and other JSONL-based agents
	return extractUserPromptsFromLines(strings.Split(content, "\n"))
}

// calculateTokenUsage calculates token usage from raw transcript data.
// startOffset is the line number (Claude Code) or message index (Gemini CLI)
// where the current checkpoint began, allowing calculation for only the portion
// of the transcript since the last checkpoint.
func calculateTokenUsage(agentType agent.AgentType, data []byte, startOffset int) *agent.TokenUsage {
	if len(data) == 0 {
		return &agent.TokenUsage{}
	}

	// Try Gemini format first if agentType is Gemini, or as fallback if Unknown
	if agentType == agent.AgentTypeGemini || agentType == agent.AgentTypeUnknown {
		// Attempt to parse as Gemini JSON
		transcript, err := geminicli.ParseTranscript(data)
		if err == nil && transcript != nil && len(transcript.Messages) > 0 {
			return geminicli.CalculateTokenUsage(data, startOffset)
		}
		// If agentType is explicitly Gemini but parsing failed, return empty usage
		if agentType == agent.AgentTypeGemini {
			return &agent.TokenUsage{}
		}
		// Otherwise fall through to JSONL parsing for Unknown type
	}

	// Claude Code and other JSONL-based agents
	lines, err := claudecode.ParseTranscript(data)
	if err != nil || len(lines) == 0 {
		return &agent.TokenUsage{}
	}
	// Slice transcript lines to only include checkpoint portion
	if startOffset > 0 && startOffset < len(lines) {
		lines = lines[startOffset:]
	}
	return claudecode.CalculateTokenUsage(lines)
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
		const maxDisplayPromptLen = 500
		displayPrompt := prompt
		if len(displayPrompt) > maxDisplayPromptLen {
			displayPrompt = displayPrompt[:maxDisplayPromptLen] + "..."
		}
		buf.WriteString(fmt.Sprintf("### Prompt %d\n\n", i+1))
		buf.WriteString(displayPrompt)
		buf.WriteString("\n\n")
	}

	return []byte(buf.String())
}

// CondenseSessionByID force-condenses a session by its ID and cleans up.
// This is used by "entire doctor" to salvage stuck sessions.
func (s *ManualCommitStrategy) CondenseSessionByID(sessionID string) error {
	ctx := logging.WithComponent(context.Background(), "condense-by-id")

	// Load session state
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Open repository
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Generate a checkpoint ID
	checkpointID, err := id.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate checkpoint ID: %w", err)
	}

	// Check if shadow branch exists (required for condensation)
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	_, refErr := repo.Reference(refName, true)
	hasShadowBranch := refErr == nil

	if !hasShadowBranch {
		// No shadow branch means no checkpoint data to condense.
		// Just clean up the state file.
		logging.Info(ctx, "no shadow branch for session, clearing state only",
			slog.String("session_id", sessionID),
			slog.String("shadow_branch", shadowBranchName),
		)
		if err := s.clearSessionState(sessionID); err != nil {
			return fmt.Errorf("failed to clear session state: %w", err)
		}
		return nil
	}

	// Condense the session
	result, err := s.CondenseSession(repo, checkpointID, state)
	if err != nil {
		return fmt.Errorf("failed to condense session: %w", err)
	}

	logging.Info(ctx, "session condensed by ID",
		slog.String("session_id", sessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
		slog.Int("checkpoints_condensed", result.CheckpointsCount),
	)

	// Update session state: reset step count and transition to idle
	state.StepCount = 0
	state.CheckpointTranscriptStart = result.TotalTranscriptLines
	state.Phase = session.PhaseIdle
	state.LastCheckpointID = checkpointID
	state.PendingCheckpointID = "" // Clear after condensation (amend handler uses LastCheckpointID)
	state.AttributionBaseCommit = state.BaseCommit
	state.PromptAttributions = nil
	state.PendingPromptAttribution = nil

	if err := s.saveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	// Clean up shadow branch if no other sessions need it
	if err := s.cleanupShadowBranchIfUnused(repo, shadowBranchName, sessionID); err != nil {
		logging.Warn(ctx, "failed to clean up shadow branch",
			slog.String("shadow_branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
		// Non-fatal: condensation succeeded, shadow branch cleanup is best-effort
	}

	return nil
}

// cleanupShadowBranchIfUnused deletes a shadow branch if no other active sessions reference it.
func (s *ManualCommitStrategy) cleanupShadowBranchIfUnused(_ *git.Repository, shadowBranchName, excludeSessionID string) error {
	// List all session states to check if any other session uses this shadow branch
	allStates, err := s.listAllSessionStates()
	if err != nil {
		return fmt.Errorf("failed to list session states: %w", err)
	}

	for _, state := range allStates {
		if state.SessionID == excludeSessionID {
			continue
		}
		otherShadow := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		if otherShadow == shadowBranchName && state.StepCount > 0 {
			// Another session still needs this shadow branch
			return nil
		}
	}

	// No other sessions need it, delete the shadow branch via CLI
	// (go-git v5's RemoveReference doesn't persist with packed refs/worktrees)
	if err := DeleteBranchCLI(shadowBranchName); err != nil {
		// Branch already gone is not an error
		if errors.Is(err, ErrBranchNotFound) {
			return nil
		}
		return fmt.Errorf("failed to remove shadow branch: %w", err)
	}
	return nil
}
