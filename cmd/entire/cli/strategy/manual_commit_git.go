package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// SaveChanges saves a checkpoint to the shadow branch.
// Uses checkpoint.GitStore.WriteTemporary for git operations.
func (s *ManualCommitStrategy) SaveChanges(ctx SaveContext) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Extract session ID from metadata dir
	sessionID := filepath.Base(ctx.MetadataDir)

	// Load or initialize session state
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	// Initialize if state is nil OR BaseCommit is empty (can happen with partial state from warnings)
	if state == nil || state.BaseCommit == "" {
		// Preserve existing AgentType if we have a partial state, otherwise use default
		agentType := DefaultAgentType
		if state != nil && state.AgentType != "" {
			agentType = state.AgentType
		}
		state, err = s.initializeSession(repo, sessionID, agentType, "") // No transcript path in fallback
		if err != nil {
			return fmt.Errorf("failed to initialize session: %w", err)
		}
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Check if shadow branch exists to report whether we created it
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit)
	branchExisted := store.ShadowBranchExists(state.BaseCommit)

	// Check if shadow branch should be reset (flag set at prompt start)
	wasReset := false
	if state.ShouldResetShadowBranch && branchExisted {
		logCtx := logging.WithComponent(context.Background(), "checkpoint")
		logging.Info(logCtx, "resetting shadow branch (no overlap at prompt start)",
			slog.String("shadow_branch", shadowBranchName),
		)

		// Delete the shadow branch
		branchRef := plumbing.NewBranchReferenceName(shadowBranchName)
		if err := repo.Storer.RemoveReference(branchRef); err != nil {
			if err != plumbing.ErrReferenceNotFound {
				return fmt.Errorf("failed to reset shadow branch: %w", err)
			}
		}

		// Reset session state for fresh start
		state.CheckpointCount = 0
		state.FilesTouched = nil
		state.PromptAttributions = nil
		branchExisted = false // Will be created fresh
		wasReset = true

		// Clear the flag after using it
		state.ShouldResetShadowBranch = false
	}

	// Use the pending attribution calculated at prompt start (in InitializeSession)
	// This was calculated BEFORE the agent made changes, so it accurately captures user edits
	var promptAttr PromptAttribution
	if state.PendingPromptAttribution != nil {
		promptAttr = *state.PendingPromptAttribution
		state.PendingPromptAttribution = nil // Clear after use
	} else {
		// No pending attribution (e.g., first checkpoint or session initialized without it)
		promptAttr = PromptAttribution{CheckpointNumber: state.CheckpointCount + 1}
	}

	// Log the prompt attribution for debugging
	attrLogCtx := logging.WithComponent(context.Background(), "attribution")
	logging.Debug(attrLogCtx, "prompt attribution at checkpoint save",
		slog.Int("checkpoint_number", promptAttr.CheckpointNumber),
		slog.Int("user_added", promptAttr.UserLinesAdded),
		slog.Int("user_removed", promptAttr.UserLinesRemoved),
		slog.Int("agent_added", promptAttr.AgentLinesAdded),
		slog.Int("agent_removed", promptAttr.AgentLinesRemoved),
		slog.String("session_id", sessionID))

	// Use WriteTemporary to create the checkpoint
	// IsFirstCheckpoint should only be true for the VERY FIRST checkpoint of the session,
	// not after a reset (which happens within an existing session)
	isFirstCheckpointOfSession := state.CheckpointCount == 0 && !wasReset
	result, err := store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        state.BaseCommit,
		ModifiedFiles:     ctx.ModifiedFiles,
		NewFiles:          ctx.NewFiles,
		DeletedFiles:      ctx.DeletedFiles,
		MetadataDir:       ctx.MetadataDir,
		MetadataDirAbs:    ctx.MetadataDirAbs,
		CommitMessage:     ctx.CommitMessage,
		AuthorName:        ctx.AuthorName,
		AuthorEmail:       ctx.AuthorEmail,
		IsFirstCheckpoint: isFirstCheckpointOfSession,
	})
	if err != nil {
		return fmt.Errorf("failed to write temporary checkpoint: %w", err)
	}

	// If checkpoint was skipped due to deduplication (no changes), return early
	if result.Skipped {
		logCtx := logging.WithComponent(context.Background(), "checkpoint")
		logging.Info(logCtx, "checkpoint skipped (no changes)",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_type", "session"),
			slog.Int("checkpoint_count", state.CheckpointCount),
			slog.String("shadow_branch", shadowBranchName),
		)
		fmt.Fprintf(os.Stderr, "Skipped checkpoint (no changes since last checkpoint)\n")
		return nil
	}

	// Update session state
	state.CheckpointCount++

	// Store the prompt attribution we calculated before saving
	state.PromptAttributions = append(state.PromptAttributions, promptAttr)

	// Track touched files (modified, new, and deleted)
	state.FilesTouched = mergeFilesTouched(state.FilesTouched, ctx.ModifiedFiles, ctx.NewFiles, ctx.DeletedFiles)

	// On first checkpoint, store the initial transcript position
	if isFirstCheckpointOfSession {
		state.TranscriptLinesAtStart = ctx.TranscriptLinesAtStart
		state.TranscriptIdentifierAtStart = ctx.TranscriptIdentifierAtStart
	}

	// Accumulate token usage
	if ctx.TokenUsage != nil {
		state.TokenUsage = accumulateTokenUsage(state.TokenUsage, ctx.TokenUsage)
	}

	// Save updated state
	if err := s.saveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	if !branchExisted {
		fmt.Fprintf(os.Stderr, "Created shadow branch '%s' and committed changes\n", shadowBranchName)
	} else {
		fmt.Fprintf(os.Stderr, "Committed changes to shadow branch '%s'\n", shadowBranchName)
	}

	// Log checkpoint creation
	logCtx := logging.WithComponent(context.Background(), "checkpoint")
	logging.Info(logCtx, "checkpoint saved",
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_type", "session"),
		slog.Int("checkpoint_count", state.CheckpointCount),
		slog.Int("modified_files", len(ctx.ModifiedFiles)),
		slog.Int("new_files", len(ctx.NewFiles)),
		slog.Int("deleted_files", len(ctx.DeletedFiles)),
		slog.String("shadow_branch", shadowBranchName),
		slog.Bool("branch_created", !branchExisted),
	)

	return nil
}

// SaveTaskCheckpoint saves a task checkpoint to the shadow branch.
// Uses checkpoint.GitStore.WriteTemporaryTask for git operations.
func (s *ManualCommitStrategy) SaveTaskCheckpoint(ctx TaskCheckpointContext) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Load session state
	state, err := s.loadSessionState(ctx.SessionID)
	if err != nil || state == nil || state.BaseCommit == "" {
		// Initialize if needed (including if BaseCommit is empty from partial warning state)
		// Preserve existing AgentType if we have a partial state, otherwise use default
		agentType := DefaultAgentType
		if state != nil && state.AgentType != "" {
			agentType = state.AgentType
		}
		state, err = s.initializeSession(repo, ctx.SessionID, agentType, "") // No transcript path in fallback
		if err != nil {
			return fmt.Errorf("failed to initialize session for task checkpoint: %w", err)
		}
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Check if shadow branch exists to report whether we created it
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit)
	branchExisted := store.ShadowBranchExists(state.BaseCommit)

	// Compute metadata paths for commit message
	sessionMetadataDir := paths.SessionMetadataDirFromEntireID(ctx.SessionID)
	taskMetadataDir := TaskMetadataDir(sessionMetadataDir, ctx.ToolUseID)

	// Generate commit message
	shortToolUseID := ctx.ToolUseID
	if len(shortToolUseID) > 12 {
		shortToolUseID = shortToolUseID[:12]
	}

	var messageSubject string
	if ctx.IsIncremental {
		messageSubject = FormatIncrementalSubject(
			ctx.IncrementalType,
			ctx.SubagentType,
			ctx.TaskDescription,
			ctx.TodoContent,
			ctx.IncrementalSequence,
			shortToolUseID,
		)
	} else {
		messageSubject = FormatSubagentEndMessage(ctx.SubagentType, ctx.TaskDescription, shortToolUseID)
	}
	commitMsg := trailers.FormatShadowTaskCommit(
		messageSubject,
		taskMetadataDir,
		ctx.SessionID,
	)

	// Use WriteTemporaryTask to create the checkpoint
	_, err = store.WriteTemporaryTask(context.Background(), checkpoint.WriteTemporaryTaskOptions{
		SessionID:              ctx.SessionID,
		BaseCommit:             state.BaseCommit,
		ToolUseID:              ctx.ToolUseID,
		AgentID:                ctx.AgentID,
		ModifiedFiles:          ctx.ModifiedFiles,
		NewFiles:               ctx.NewFiles,
		DeletedFiles:           ctx.DeletedFiles,
		TranscriptPath:         ctx.TranscriptPath,
		SubagentTranscriptPath: ctx.SubagentTranscriptPath,
		CheckpointUUID:         ctx.CheckpointUUID,
		CommitMessage:          commitMsg,
		AuthorName:             ctx.AuthorName,
		AuthorEmail:            ctx.AuthorEmail,
		IsIncremental:          ctx.IsIncremental,
		IncrementalSequence:    ctx.IncrementalSequence,
		IncrementalType:        ctx.IncrementalType,
		IncrementalData:        ctx.IncrementalData,
	})
	if err != nil {
		return fmt.Errorf("failed to write task checkpoint: %w", err)
	}

	// Track touched files (modified, new, and deleted)
	state.FilesTouched = mergeFilesTouched(state.FilesTouched, ctx.ModifiedFiles, ctx.NewFiles, ctx.DeletedFiles)

	// Save updated state
	if err := s.saveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	if !branchExisted {
		fmt.Fprintf(os.Stderr, "Created shadow branch '%s' and committed task checkpoint\n", shadowBranchName)
	} else {
		fmt.Fprintf(os.Stderr, "Committed task checkpoint to shadow branch '%s'\n", shadowBranchName)
	}

	// Log task checkpoint creation
	logCtx := logging.WithComponent(context.Background(), "checkpoint")
	attrs := []any{
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_type", "task"),
		slog.String("checkpoint_uuid", ctx.CheckpointUUID),
		slog.String("tool_use_id", ctx.ToolUseID),
		slog.String("subagent_type", ctx.SubagentType),
		slog.Int("modified_files", len(ctx.ModifiedFiles)),
		slog.Int("new_files", len(ctx.NewFiles)),
		slog.Int("deleted_files", len(ctx.DeletedFiles)),
		slog.String("shadow_branch", shadowBranchName),
		slog.Bool("branch_created", !branchExisted),
	}
	if ctx.IsIncremental {
		attrs = append(attrs,
			slog.Bool("is_incremental", true),
			slog.String("incremental_type", ctx.IncrementalType),
			slog.Int("incremental_sequence", ctx.IncrementalSequence),
		)
	}
	logging.Info(logCtx, "task checkpoint saved", attrs...)

	return nil
}

// mergeFilesTouched merges multiple file lists into existing touched files, deduplicating.
func mergeFilesTouched(existing []string, fileLists ...[]string) []string {
	seen := make(map[string]bool)
	for _, f := range existing {
		seen[f] = true
	}

	for _, list := range fileLists {
		for _, f := range list {
			seen[f] = true
		}
	}

	result := make([]string, 0, len(seen))
	for f := range seen {
		result = append(result, f)
	}

	// Sort for deterministic output
	sort.Strings(result)
	return result
}

// accumulateTokenUsage adds new token usage to existing accumulated usage.
// If existing is nil, returns a copy of incoming. If incoming is nil, returns existing unchanged.
func accumulateTokenUsage(existing, incoming *agent.TokenUsage) *agent.TokenUsage {
	if incoming == nil {
		return existing
	}
	if existing == nil {
		// Return a copy to avoid sharing the pointer
		return &agent.TokenUsage{
			InputTokens:         incoming.InputTokens,
			CacheCreationTokens: incoming.CacheCreationTokens,
			CacheReadTokens:     incoming.CacheReadTokens,
			OutputTokens:        incoming.OutputTokens,
			APICallCount:        incoming.APICallCount,
			SubagentTokens:      incoming.SubagentTokens,
		}
	}

	// Accumulate values
	existing.InputTokens += incoming.InputTokens
	existing.CacheCreationTokens += incoming.CacheCreationTokens
	existing.CacheReadTokens += incoming.CacheReadTokens
	existing.OutputTokens += incoming.OutputTokens
	existing.APICallCount += incoming.APICallCount

	// Accumulate subagent tokens if present
	if incoming.SubagentTokens != nil {
		existing.SubagentTokens = accumulateTokenUsage(existing.SubagentTokens, incoming.SubagentTokens)
	}

	return existing
}

// deleteShadowBranch deletes a shadow branch by name using an existing repo handle.
// Returns nil if the branch doesn't exist (idempotent).
func deleteShadowBranch(repo *git.Repository, branchName string) error {
	refName := plumbing.NewBranchReferenceName(branchName)

	// Check if reference exists
	ref, err := repo.Reference(refName, true)
	if err != nil {
		// Branch doesn't exist - nothing to delete (idempotent)
		return nil //nolint:nilerr // Not an error condition - branch already gone
	}

	// Delete the reference
	if err := repo.Storer.RemoveReference(ref.Name()); err != nil {
		return fmt.Errorf("failed to delete branch %s: %w", branchName, err)
	}

	return nil
}
