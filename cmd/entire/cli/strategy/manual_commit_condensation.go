package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// listCheckpoints returns all checkpoints from the metadata branch.
// Uses checkpoint.GitStore.ListCommitted() for reading from entire/checkpoints/v1.
func (s *ManualCommitStrategy) listCheckpoints(ctx context.Context) ([]CheckpointInfo, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	committed, err := store.ListCommitted(ctx)
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

// getCheckpointLog returns the transcript for a specific checkpoint ID.
// Uses checkpoint.GitStore.ReadCommitted() for reading from entire/checkpoints/v1.
func (s *ManualCommitStrategy) getCheckpointLog(ctx context.Context, checkpointID id.CheckpointID) ([]byte, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	content, err := store.ReadLatestSessionContent(ctx, checkpointID)
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

// condenseOpts provides pre-resolved git objects to avoid redundant reads.
type condenseOpts struct {
	shadowRef      *plumbing.Reference // Pre-resolved shadow branch ref (nil = resolve from repo)
	headTree       *object.Tree        // Pre-resolved HEAD tree (passed through to calculateSessionAttributions)
	repoDir        string              // Repository worktree path for git CLI commands
	headCommitHash string              // HEAD commit hash (passed through for attribution)
}

// CondenseSession condenses a session's shadow branch to permanent storage.
// checkpointID is the 12-hex-char value from the Entire-Checkpoint trailer.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
// Uses checkpoint.GitStore.WriteCommitted for the git operations.
//
// For mid-session commits (no Stop/SaveStep called yet), the shadow branch may not exist.
// In this case, data is extracted from the live transcript instead.
func (s *ManualCommitStrategy) CondenseSession(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID, state *SessionState, committedFiles map[string]struct{}, opts ...condenseOpts) (*CondenseResult, error) {
	ag, _ := agent.GetByAgentType(state.AgentType) //nolint:errcheck // ag may be nil for unknown agent types; callers use type assertions so nil is safe
	var o condenseOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	// Get shadow branch — use pre-resolved ref if available, otherwise resolve from repo.
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	ref := o.shadowRef
	var hasShadowBranch bool
	if ref != nil {
		hasShadowBranch = true
	} else {
		refName := plumbing.NewBranchReferenceName(shadowBranchName)
		var err error
		ref, err = repo.Reference(refName, true)
		hasShadowBranch = err == nil
	}

	// Re-resolve transcript path before any reads — handles agents that relocate
	// transcripts mid-session (e.g., Cursor CLI flat → nested layout change).
	// Errors are ignored; downstream readers handle missing transcripts gracefully.
	resolveTranscriptPath(state) //nolint:errcheck,gosec // best-effort; downstream readers handle missing files

	var sessionData *ExtractedSessionData
	if hasShadowBranch {
		var extractErr error
		sessionData, extractErr = s.extractSessionData(ctx, repo, ref.Hash(), state.SessionID, state.FilesTouched, state.AgentType, state.TranscriptPath, state.CheckpointTranscriptStart, state.Phase.IsActive())
		if extractErr != nil {
			return nil, fmt.Errorf("failed to extract session data: %w", extractErr)
		}
	} else {
		if state.TranscriptPath == "" {
			return nil, errors.New("shadow branch not found and no live transcript available")
		}
		if state.Phase.IsActive() {
			prepareTranscriptIfNeeded(ctx, ag, state.TranscriptPath)
		}
		var extractErr error
		sessionData, extractErr = s.extractSessionDataFromLiveTranscript(ctx, state)
		if extractErr != nil {
			return nil, fmt.Errorf("failed to extract session data from live transcript: %w", extractErr)
		}
	}

	// Backfill session state token usage from the freshly-extracted transcript.
	// Copilot CLI writes session.shutdown after the hooks return, so by condensation
	// time we can recover the authoritative full-session total from the transcript
	// while keeping checkpoint metadata scoped to CheckpointTranscriptStart.
	if backfillUsage := sessionStateBackfillTokenUsage(ctx, ag, state.AgentType, sessionData.Transcript, sessionData.TokenUsage); backfillUsage != nil {
		state.TokenUsage = backfillUsage
	}

	// For 1:1 checkpoint model: filter files_touched to only include files actually
	// committed in this specific commit. This ensures each checkpoint represents
	// exactly the files in that commit, not all files mentioned in the transcript.
	if len(committedFiles) > 0 {
		hadFilesBeforeFiltering := len(sessionData.FilesTouched) > 0

		if hadFilesBeforeFiltering {
			filtered := make([]string, 0, len(sessionData.FilesTouched))
			for _, f := range sessionData.FilesTouched {
				if _, ok := committedFiles[f]; ok {
					filtered = append(filtered, f)
				}
			}
			sessionData.FilesTouched = filtered
		}

		if len(sessionData.FilesTouched) == 0 && !hadFilesBeforeFiltering {
			sessionData.FilesTouched = make([]string, 0, len(committedFiles))
			for f := range committedFiles {
				sessionData.FilesTouched = append(sessionData.FilesTouched, f)
			}
		}
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Get author info
	authorName, authorEmail := GetGitAuthorFromRepo(repo)

	// Determine attribution base commit
	attrBase := state.AttributionBaseCommit
	if attrBase == "" {
		attrBase = state.BaseCommit
	}

	attribution := calculateSessionAttributions(ctx, repo, ref, sessionData, state, attributionOpts{
		headTree:              o.headTree,
		repoDir:               o.repoDir,
		attributionBaseCommit: attrBase,
		headCommitHash:        o.headCommitHash,
	})

	// Get current branch name
	branchName := GetCurrentBranchName(repo)

	// Generate summary if enabled
	var summary *cpkg.Summary
	if settings.IsSummarizeEnabled(ctx) && len(sessionData.Transcript) > 0 {
		summarizeCtx := logging.WithComponent(ctx, "summarize")

		var scopedTranscript []byte
		switch state.AgentType {
		case agent.AgentTypeGemini:
			scoped, sliceErr := geminicli.SliceFromMessage(sessionData.Transcript, state.CheckpointTranscriptStart)
			if sliceErr != nil {
				logging.Warn(summarizeCtx, "failed to scope Gemini transcript for summary",
					slog.String("session_id", state.SessionID),
					slog.String("error", sliceErr.Error()))
			}
			scopedTranscript = scoped
		case agent.AgentTypeOpenCode:
			scoped, sliceErr := opencode.SliceFromMessage(sessionData.Transcript, state.CheckpointTranscriptStart)
			if sliceErr != nil {
				logging.Warn(summarizeCtx, "failed to scope OpenCode transcript for summary",
					slog.String("session_id", state.SessionID),
					slog.String("error", sliceErr.Error()))
			}
			scopedTranscript = scoped
		case agent.AgentTypeClaudeCode, agent.AgentTypeCursor, agent.AgentTypeFactoryAIDroid, agent.AgentTypeUnknown:
			scopedTranscript = transcript.SliceFromLine(sessionData.Transcript, state.CheckpointTranscriptStart)
		}
		if len(scopedTranscript) > 0 {
			var err error
			summary, err = summarize.GenerateFromTranscript(summarizeCtx, scopedTranscript, sessionData.FilesTouched, state.AgentType, nil)
			if err != nil {
				logging.Warn(summarizeCtx, "summary generation failed",
					slog.String("session_id", state.SessionID),
					slog.String("error", err.Error()))
			} else {
				logging.Info(summarizeCtx, "summary generated",
					slog.String("session_id", state.SessionID))
			}
		}
	}

	// Build write options (shared by v1 and v2)
	writeOpts := cpkg.WriteCommittedOptions{
		CheckpointID:                checkpointID,
		SessionID:                   state.SessionID,
		Strategy:                    StrategyNameManualCommit,
		Branch:                      branchName,
		Transcript:                  sessionData.Transcript,
		Prompts:                     sessionData.Prompts,
		FilesTouched:                sessionData.FilesTouched,
		CheckpointsCount:            state.StepCount,
		EphemeralBranch:             shadowBranchName,
		AuthorName:                  authorName,
		AuthorEmail:                 authorEmail,
		Agent:                       state.AgentType,
		Model:                       state.ModelName,
		TurnID:                      state.TurnID,
		TranscriptIdentifierAtStart: state.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   state.CheckpointTranscriptStart,
		TokenUsage:                  sessionData.TokenUsage,
		SessionMetrics:              buildSessionMetrics(state),
		InitialAttribution:          attribution,
		Summary:                     summary,
	}

	// Write checkpoint metadata to v1 branch
	if err := store.WriteCommitted(ctx, writeOpts); err != nil {
		return nil, fmt.Errorf("failed to write checkpoint metadata: %w", err)
	}

	writeCommittedV2IfEnabled(ctx, repo, writeOpts)

	return &CondenseResult{
		CheckpointID:         checkpointID,
		SessionID:            state.SessionID,
		CheckpointsCount:     state.StepCount,
		FilesTouched:         sessionData.FilesTouched,
		Prompts:              sessionData.Prompts,
		TotalTranscriptLines: sessionData.FullTranscriptLines,
		Transcript:           sessionData.Transcript,
	}, nil
}

// buildSessionMetrics creates a SessionMetrics from session state if any metrics are available.
// Returns nil if no hook-provided metrics exist (e.g., for agents that don't report them).
func buildSessionMetrics(state *SessionState) *cpkg.SessionMetrics {
	if state.SessionDurationMs == 0 && state.SessionTurnCount == 0 && state.ContextTokens == 0 && state.ContextWindowSize == 0 {
		return nil
	}
	return &cpkg.SessionMetrics{
		DurationMs:        state.SessionDurationMs,
		TurnCount:         state.SessionTurnCount,
		ContextTokens:     state.ContextTokens,
		ContextWindowSize: state.ContextWindowSize,
	}
}

func hasTokenUsageData(usage *agent.TokenUsage) bool {
	if usage == nil {
		return false
	}

	if usage.InputTokens > 0 || usage.CacheCreationTokens > 0 || usage.CacheReadTokens > 0 || usage.OutputTokens > 0 || usage.APICallCount > 0 {
		return true
	}

	return hasTokenUsageData(usage.SubagentTokens)
}

// sessionStateBackfillTokenUsage returns the best session-level token usage to
// persist in session state after condensation.
func sessionStateBackfillTokenUsage(ctx context.Context, ag agent.Agent, agentType types.AgentType, transcript []byte, checkpointUsage *agent.TokenUsage) *agent.TokenUsage {
	if agentType == agent.AgentTypeCopilotCLI && len(transcript) > 0 {
		fullSessionUsage := agent.CalculateTokenUsage(ctx, ag, transcript, 0, "")
		if hasTokenUsageData(fullSessionUsage) {
			return fullSessionUsage
		}
		logging.Debug(ctx, "copilot-cli: full-session token read produced no data, falling back to checkpoint usage")
	}

	if agentType == agent.AgentTypeCopilotCLI && hasTokenUsageData(checkpointUsage) {
		return checkpointUsage
	}

	if checkpointUsage != nil && checkpointUsage.InputTokens > 0 {
		return checkpointUsage
	}

	return nil
}

// attributionOpts provides pre-resolved git objects to avoid redundant reads.
type attributionOpts struct {
	headTree              *object.Tree // HEAD commit tree (already resolved by PostCommit)
	shadowTree            *object.Tree // Shadow branch tree (already resolved by PostCommit)
	repoDir               string       // Repository worktree path for git CLI commands
	attributionBaseCommit string       // Base commit hash for non-agent file detection (empty = fall back to go-git tree walk)
	headCommitHash        string       // HEAD commit hash for non-agent file detection (empty = fall back to go-git tree walk)
}

func calculateSessionAttributions(ctx context.Context, repo *git.Repository, shadowRef *plumbing.Reference, sessionData *ExtractedSessionData, state *SessionState, opts ...attributionOpts) *cpkg.InitialAttribution {
	// Calculate initial attribution using accumulated prompt attribution data.
	// This uses user edits captured at each prompt start (before agent works),
	// plus any user edits after the final checkpoint (shadow → head).
	//
	// When shadowRef is nil (agent committed mid-turn before SaveStep),
	// HEAD is used as the shadow tree. This is correct because the agent's
	// commit IS HEAD — there are no user edits between agent work and commit.
	logCtx := logging.WithComponent(ctx, "attribution")

	var o attributionOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	headTree := o.headTree
	if headTree == nil {
		headRef, headErr := repo.Head()
		if headErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD",
				slog.String("error", headErr.Error()))
			return nil
		}

		headCommit, commitErr := repo.CommitObject(headRef.Hash())
		if commitErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD commit",
				slog.String("error", commitErr.Error()))
			return nil
		}

		var treeErr error
		headTree, treeErr = headCommit.Tree()
		if treeErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD tree",
				slog.String("error", treeErr.Error()))
			return nil
		}
	}

	// Get shadow tree: from pre-resolved cache, shadow branch, or HEAD (agent committed directly).
	shadowTree := o.shadowTree
	if shadowTree == nil {
		if shadowRef != nil {
			shadowCommit, shadowErr := repo.CommitObject(shadowRef.Hash())
			if shadowErr != nil {
				logging.Debug(logCtx, "attribution skipped: failed to get shadow commit",
					slog.String("error", shadowErr.Error()),
					slog.String("shadow_ref", shadowRef.Hash().String()))
				return nil
			}
			var shadowTreeErr error
			shadowTree, shadowTreeErr = shadowCommit.Tree()
			if shadowTreeErr != nil {
				logging.Debug(logCtx, "attribution skipped: failed to get shadow tree",
					slog.String("error", shadowTreeErr.Error()))
				return nil
			}
		} else {
			// No shadow branch: agent committed mid-turn. Use HEAD as shadow
			// because the agent's work is the commit itself.
			logging.Debug(logCtx, "attribution: using HEAD as shadow (no shadow branch)")
			shadowTree = headTree
		}
	}

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

	attribution := CalculateAttributionWithAccumulated(
		ctx,
		baseTree,
		shadowTree,
		headTree,
		sessionData.FilesTouched,
		state.PromptAttributions,
		o.repoDir,
		o.attributionBaseCommit,
		o.headCommitHash,
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

	return attribution
}

// extractSessionData extracts session data from the shadow branch.
// filesTouched is the list of files tracked during the session (from SessionState.FilesTouched).
// agentType identifies the agent (e.g., "Gemini CLI", "Claude Code") to determine transcript format.
// liveTranscriptPath, when non-empty and readable, is preferred over the shadow branch copy.
// This handles the case where SaveStep was skipped (no code changes) but the transcript
// continued growing — the shadow branch copy would be stale.
// checkpointTranscriptStart is the line offset (Claude) or message index (Gemini) where the current checkpoint began.
func (s *ManualCommitStrategy) extractSessionData(ctx context.Context, repo *git.Repository, shadowRef plumbing.Hash, sessionID string, filesTouched []string, agentType types.AgentType, liveTranscriptPath string, checkpointTranscriptStart int, isActive bool) (*ExtractedSessionData, error) {
	ag, _ := agent.GetByAgentType(agentType) //nolint:errcheck // ag may be nil for unknown agent types; callers use type assertions so nil is safe
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
	// (SaveStep is only called when there are file modifications).
	var fullTranscript string
	if liveTranscriptPath != "" {
		// Ensure transcript file exists (OpenCode creates it lazily via `opencode export`).
		// Only wait for flush when the session is active — for idle/ended sessions the
		// transcript is already fully flushed (the Stop hook completed the flush).
		if isActive {
			prepareTranscriptIfNeeded(ctx, ag, liveTranscriptPath)
		}
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
		// Read prompts from shadow branch tree (source of truth after SaveStep)
		if file, fileErr := tree.File(metadataDir + "/" + paths.PromptFileName); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil && content != "" {
				data.Prompts = splitPromptContent(content)
			}
		}
		// Filesystem fallback (written at turn start, covers mid-turn commits)
		if len(data.Prompts) == 0 {
			data.Prompts = readPromptsFromFilesystem(ctx, sessionID)
		}
	}

	// Use tracked files from session state (not all files in tree)
	data.FilesTouched = filesTouched

	// Calculate token usage from the extracted transcript portion
	if len(data.Transcript) > 0 {
		data.TokenUsage = agent.CalculateTokenUsage(ctx, ag, data.Transcript, checkpointTranscriptStart, "") //TODO: why do we not use here subagents dir?
	}

	return data, nil
}

// extractSessionDataFromLiveTranscript extracts session data directly from the live transcript file.
// This is used for mid-session commits where no shadow branch exists yet.
func (s *ManualCommitStrategy) extractSessionDataFromLiveTranscript(ctx context.Context, state *SessionState) (*ExtractedSessionData, error) {
	data := &ExtractedSessionData{}

	ag, _ := agent.GetByAgentType(state.AgentType) //nolint:errcheck // ag may be nil for unknown agent types; callers use type assertions so nil is safe

	// Resolve the transcript path (handles agents that relocate mid-session).
	transcriptPath, resolveErr := resolveTranscriptPath(state)
	if resolveErr != nil {
		return nil, resolveErr
	}

	liveData, err := os.ReadFile(transcriptPath) //nolint:gosec // path validated by resolveTranscriptPath
	if err != nil {
		return nil, fmt.Errorf("failed to read live transcript: %w", err)
	}

	if len(liveData) == 0 {
		return nil, errors.New("live transcript is empty")
	}

	fullTranscript := string(liveData)
	data.Transcript = liveData
	data.FullTranscriptLines = countTranscriptItems(state.AgentType, fullTranscript)
	data.Prompts = readPromptsFromFilesystem(ctx, state.SessionID)

	// Resolve files touched: prefers hook-populated state, falls back to transcript extraction
	data.FilesTouched = s.resolveFilesTouched(ctx, state)

	// Calculate token usage from the extracted transcript portion
	if len(data.Transcript) > 0 {
		data.TokenUsage = agent.CalculateTokenUsage(ctx, ag, data.Transcript, state.CheckpointTranscriptStart, "") //TODO: why do we not use here subagents dir?
	}

	return data, nil
}

// countTranscriptItems counts lines (JSONL) or messages (JSON) in a transcript.
// For Claude Code and JSONL-based agents, this counts lines.
// For Gemini CLI, OpenCode, and JSON-based agents, this counts messages.
// Returns 0 if the content is empty or malformed.
func countTranscriptItems(agentType types.AgentType, content string) int {
	if content == "" {
		return 0
	}

	// OpenCode uses export JSON format with {"info": {...}, "messages": [...]}
	if agentType == agent.AgentTypeOpenCode {
		session, err := opencode.ParseExportSession([]byte(content))
		if err == nil && session != nil {
			return len(session.Messages)
		}
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
func extractUserPrompts(agentType types.AgentType, content string) []string {
	if content == "" {
		return nil
	}

	// Droid has its own envelope format — use its parser to normalize first
	if agentType == agent.AgentTypeFactoryAIDroid {
		lines, _, err := factoryaidroid.ParseDroidTranscriptFromBytes([]byte(content), 0)
		if err != nil {
			return nil
		}
		var prompts []string
		for _, line := range lines {
			if line.Type != transcript.TypeUser {
				continue
			}
			if text := transcript.ExtractUserContent(line.Message); text != "" {
				if stripped := textutil.StripIDEContextTags(text); stripped != "" {
					prompts = append(prompts, stripped)
				}
			}
		}
		return prompts
	}

	// OpenCode uses JSONL with a different per-line schema than Claude Code
	if agentType == agent.AgentTypeOpenCode {
		prompts, err := opencode.ExtractAllUserPrompts([]byte(content))
		if err == nil && len(prompts) > 0 {
			cleaned := make([]string, 0, len(prompts))
			for _, prompt := range prompts {
				if stripped := textutil.StripIDEContextTags(prompt); stripped != "" {
					cleaned = append(cleaned, stripped)
				}
			}
			return cleaned
		}
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

		// Check for user message:
		// - Claude Code uses "type": "human" or "type": "user"
		// - Cursor uses "role": "user"
		msgType, _ := entry["type"].(string) //nolint:errcheck // type assertion on interface{} from JSON
		msgRole, _ := entry["role"].(string) //nolint:errcheck // type assertion on interface{} from JSON
		isUser := msgType == "human" || msgType == "user" || msgRole == "user"
		if !isUser {
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

// splitPromptContent splits prompt.txt content on the "\n\n---\n\n" separator.
// Returns nil if content is empty.
func splitPromptContent(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.Split(content, "\n\n---\n\n")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// readPromptsFromFilesystem reads prompt.txt from the filesystem session metadata directory.
// This file is written at turn start and updated at each SaveStep, providing prompt data
// even for mid-turn commits where the shadow branch may not have been updated.
func readPromptsFromFilesystem(ctx context.Context, sessionID string) []string {
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName)) //nolint:gosec // path from session ID
	if err != nil || len(data) == 0 {
		return nil
	}
	return splitPromptContent(string(data))
}

// clearFilesystemPrompt removes the filesystem prompt.txt for a session.
// Called after condensation so subsequent checkpoints start fresh.
func clearFilesystemPrompt(ctx context.Context, sessionID string) {
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return
	}
	promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
	_ = os.Remove(promptPath)
}

// CondenseSessionByID force-condenses a session by its ID and cleans up.
// This is used by "entire doctor" to salvage stuck sessions.
func (s *ManualCommitStrategy) CondenseSessionByID(ctx context.Context, sessionID string) error {
	logCtx := logging.WithComponent(ctx, "condense-by-id")

	// Load session state
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Open repository
	repo, err := OpenRepository(ctx)
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
		logging.Info(logCtx, "no shadow branch for session, clearing state only",
			slog.String("session_id", sessionID),
			slog.String("shadow_branch", shadowBranchName),
		)
		if err := s.clearSessionState(ctx, sessionID); err != nil {
			return fmt.Errorf("failed to clear session state: %w", err)
		}
		return nil
	}

	// Condense the session
	result, err := s.CondenseSession(ctx, repo, checkpointID, state, nil)
	if err != nil {
		return fmt.Errorf("failed to condense session: %w", err)
	}

	logging.Info(logCtx, "session condensed by ID",
		slog.String("session_id", sessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
		slog.Int("checkpoints_condensed", result.CheckpointsCount),
	)

	// Update session state: reset step count and transition to idle
	state.StepCount = 0
	state.CheckpointTranscriptStart = result.TotalTranscriptLines
	state.CheckpointTranscriptSize = int64(len(result.Transcript))
	state.Phase = session.PhaseIdle
	state.LastCheckpointID = checkpointID
	state.AttributionBaseCommit = state.BaseCommit
	state.PromptAttributions = nil
	state.PendingPromptAttribution = nil

	if err := s.saveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	// Clean up shadow branch if no other sessions need it
	if err := s.cleanupShadowBranchIfUnused(ctx, repo, shadowBranchName, sessionID); err != nil {
		logging.Warn(logCtx, "failed to clean up shadow branch",
			slog.String("shadow_branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
		// Non-fatal: condensation succeeded, shadow branch cleanup is best-effort
	}

	return nil
}

// cleanupShadowBranchIfUnused deletes a shadow branch if no other active sessions reference it.
func (s *ManualCommitStrategy) cleanupShadowBranchIfUnused(ctx context.Context, _ *git.Repository, shadowBranchName, excludeSessionID string) error {
	// List all session states to check if any other session uses this shadow branch
	allStates, err := s.listAllSessionStates(ctx)
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
	if err := DeleteBranchCLI(ctx, shadowBranchName); err != nil {
		// Branch already gone is not an error
		if errors.Is(err, ErrBranchNotFound) {
			return nil
		}
		return fmt.Errorf("failed to remove shadow branch: %w", err)
	}
	return nil
}

// writeCommittedV2IfEnabled writes checkpoint data to v2 refs when checkpoints_v2
// is enabled in settings. Failures are logged as warnings — v2 writes are
// best-effort during the dual-write period and must not block the v1 path.
func writeCommittedV2IfEnabled(ctx context.Context, repo *git.Repository, opts cpkg.WriteCommittedOptions) {
	if !settings.IsCheckpointsV2Enabled(ctx) {
		return
	}

	v2Store := cpkg.NewV2GitStore(repo)
	if err := v2Store.WriteCommitted(ctx, opts); err != nil {
		logging.Warn(ctx, "v2 dual-write failed",
			slog.String("checkpoint_id", opts.CheckpointID.String()),
			slog.String("error", err.Error()),
		)
	}
}
