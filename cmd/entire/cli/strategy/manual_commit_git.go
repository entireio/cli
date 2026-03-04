package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
)

// SaveStep saves a checkpoint to the shadow branch.
// Uses checkpoint.GitStore.WriteTemporary for git operations.
func (s *ManualCommitStrategy) SaveStep(ctx context.Context, step StepContext) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Extract session ID from metadata dir
	sessionID := filepath.Base(step.MetadataDir)

	// Load or initialize session state
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	// Initialize if state is nil OR BaseCommit is empty (can happen with partial state from warnings)
	if state == nil || state.BaseCommit == "" {
		agentType := resolveAgentType(step.AgentType, state)
		state, err = s.initializeSession(ctx, repo, sessionID, agentType, "", "", "") // No transcript/prompt/model in fallback
		if err != nil {
			return fmt.Errorf("failed to initialize session: %w", err)
		}
	}

	// Check if HEAD has changed (e.g., Claude did a rebase via tool call) and migrate if needed
	if err := s.migrateAndPersistIfNeeded(ctx, repo, state); err != nil {
		return err
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Check if shadow branch exists to report whether we created it
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	branchExisted := store.ShadowBranchExists(state.BaseCommit, state.WorktreeID)

	// Use the pending attribution calculated at prompt start (in InitializeSession)
	// This was calculated BEFORE the agent made changes, so it accurately captures user edits
	var promptAttr PromptAttribution
	if state.PendingPromptAttribution != nil {
		promptAttr = *state.PendingPromptAttribution
		state.PendingPromptAttribution = nil // Clear after use
	} else {
		// No pending attribution (e.g., first checkpoint or session initialized without it)
		promptAttr = PromptAttribution{CheckpointNumber: state.StepCount + 1}
	}

	// Log the prompt attribution for debugging
	attrLogCtx := logging.WithComponent(ctx, "attribution")
	logging.Debug(attrLogCtx, "prompt attribution at checkpoint save",
		slog.Int("checkpoint_number", promptAttr.CheckpointNumber),
		slog.Int("user_added", promptAttr.UserLinesAdded),
		slog.Int("user_removed", promptAttr.UserLinesRemoved),
		slog.Int("agent_added", promptAttr.AgentLinesAdded),
		slog.Int("agent_removed", promptAttr.AgentLinesRemoved),
		slog.String("session_id", sessionID))

	// Use WriteTemporary to create the checkpoint
	isFirstCheckpointOfSession := state.StepCount == 0
	result, err := store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        state.BaseCommit,
		WorktreeID:        state.WorktreeID,
		ModifiedFiles:     step.ModifiedFiles,
		NewFiles:          step.NewFiles,
		DeletedFiles:      step.DeletedFiles,
		MetadataDir:       step.MetadataDir,
		MetadataDirAbs:    step.MetadataDirAbs,
		CommitMessage:     step.CommitMessage,
		AuthorName:        step.AuthorName,
		AuthorEmail:       step.AuthorEmail,
		IsFirstCheckpoint: isFirstCheckpointOfSession,
	})
	if err != nil {
		return fmt.Errorf("failed to write temporary checkpoint: %w", err)
	}

	// If checkpoint was skipped due to deduplication (no changes), return early
	if result.Skipped {
		logCtx := logging.WithComponent(ctx, "checkpoint")
		logging.Info(logCtx, "checkpoint skipped (no changes)",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_type", "session"),
			slog.Int("checkpoint_count", state.StepCount),
			slog.String("shadow_branch", shadowBranchName),
		)
		return nil
	}

	// Update session state
	state.StepCount++

	// Note: LastCheckpointID is intentionally NOT cleared here.
	// It is set during condensation and used by handleAmendCommitMsg
	// to restore checkpoint trailers on amend operations.

	// Store the prompt attribution we calculated before saving
	state.PromptAttributions = append(state.PromptAttributions, promptAttr)

	// Track touched files (modified, new, and deleted)
	state.FilesTouched = mergeFilesTouched(state.FilesTouched, step.ModifiedFiles, step.NewFiles, step.DeletedFiles)

	// On first checkpoint, record the transcript identifier for this session
	if state.StepCount == 1 {
		state.TranscriptIdentifierAtStart = step.StepTranscriptIdentifier
	}

	// Accumulate token usage
	if step.TokenUsage != nil {
		state.TokenUsage = accumulateTokenUsage(state.TokenUsage, step.TokenUsage)
	}

	// Save updated state
	if err := s.saveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	if !branchExisted {
		logging.Info(logging.WithComponent(ctx, "checkpoint"), "created shadow branch and committed changes",
			slog.String("shadow_branch", shadowBranchName))
	} else {
		logging.Info(logging.WithComponent(ctx, "checkpoint"), "committed changes to shadow branch",
			slog.String("shadow_branch", shadowBranchName))
	}

	// Log checkpoint creation
	logCtx := logging.WithComponent(ctx, "checkpoint")
	logging.Info(logCtx, "checkpoint saved",
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_type", "session"),
		slog.Int("checkpoint_count", state.StepCount),
		slog.Int("modified_files", len(step.ModifiedFiles)),
		slog.Int("new_files", len(step.NewFiles)),
		slog.Int("deleted_files", len(step.DeletedFiles)),
		slog.String("shadow_branch", shadowBranchName),
		slog.Bool("branch_created", !branchExisted),
	)

	return nil
}

// SaveTaskStep saves a task step checkpoint to the shadow branch.
// Uses checkpoint.GitStore.WriteTemporaryTask for git operations.
func (s *ManualCommitStrategy) SaveTaskStep(ctx context.Context, step TaskStepContext) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Load session state
	state, err := s.loadSessionState(ctx, step.SessionID)
	if err != nil || state == nil || state.BaseCommit == "" {
		agentType := resolveAgentType(step.AgentType, state)
		state, err = s.initializeSession(ctx, repo, step.SessionID, agentType, "", "", "") // No transcript/prompt/model in fallback
		if err != nil {
			return fmt.Errorf("failed to initialize session for task checkpoint: %w", err)
		}
	}

	// Check if HEAD has changed (e.g., Claude did a rebase via tool call) and migrate if needed
	if err := s.migrateAndPersistIfNeeded(ctx, repo, state); err != nil {
		return err
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Check if shadow branch exists to report whether we created it
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	branchExisted := store.ShadowBranchExists(state.BaseCommit, state.WorktreeID)

	// Compute metadata paths for commit message
	sessionMetadataDir := paths.SessionMetadataDirFromSessionID(step.SessionID)
	taskMetadataDir := TaskMetadataDir(sessionMetadataDir, step.ToolUseID)

	// Generate commit message
	shortToolUseID := step.ToolUseID
	if len(shortToolUseID) > id.ShortIDLength {
		shortToolUseID = shortToolUseID[:id.ShortIDLength]
	}

	var messageSubject string
	if step.IsIncremental {
		messageSubject = FormatIncrementalSubject(
			step.IncrementalType,
			step.SubagentType,
			step.TaskDescription,
			step.TodoContent,
			step.IncrementalSequence,
			shortToolUseID,
		)
	} else {
		messageSubject = FormatSubagentEndMessage(step.SubagentType, step.TaskDescription, shortToolUseID)
	}
	commitMsg := trailers.FormatShadowTaskCommit(
		messageSubject,
		taskMetadataDir,
		step.SessionID,
	)

	// Use WriteTemporaryTask to create the checkpoint
	_, err = store.WriteTemporaryTask(ctx, checkpoint.WriteTemporaryTaskOptions{
		SessionID:              step.SessionID,
		BaseCommit:             state.BaseCommit,
		WorktreeID:             state.WorktreeID,
		ToolUseID:              step.ToolUseID,
		AgentID:                step.AgentID,
		ModifiedFiles:          step.ModifiedFiles,
		NewFiles:               step.NewFiles,
		DeletedFiles:           step.DeletedFiles,
		TranscriptPath:         step.TranscriptPath,
		SubagentTranscriptPath: step.SubagentTranscriptPath,
		CheckpointUUID:         step.CheckpointUUID,
		CommitMessage:          commitMsg,
		AuthorName:             step.AuthorName,
		AuthorEmail:            step.AuthorEmail,
		IsIncremental:          step.IsIncremental,
		IncrementalSequence:    step.IncrementalSequence,
		IncrementalType:        step.IncrementalType,
		IncrementalData:        step.IncrementalData,
	})
	if err != nil {
		return fmt.Errorf("failed to write task checkpoint: %w", err)
	}

	// Track touched files (modified, new, and deleted)
	state.FilesTouched = mergeFilesTouched(state.FilesTouched, step.ModifiedFiles, step.NewFiles, step.DeletedFiles)

	// Save updated state
	if err := s.saveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	if !branchExisted {
		logging.Info(logging.WithComponent(ctx, "checkpoint"), "created shadow branch and committed task checkpoint",
			slog.String("shadow_branch", shadowBranchName))
	} else {
		logging.Info(logging.WithComponent(ctx, "checkpoint"), "committed task checkpoint to shadow branch",
			slog.String("shadow_branch", shadowBranchName))
	}

	// Log task checkpoint creation
	logCtx := logging.WithComponent(ctx, "checkpoint")
	attrs := []any{
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_type", "task"),
		slog.String("checkpoint_uuid", step.CheckpointUUID),
		slog.String("tool_use_id", step.ToolUseID),
		slog.String("subagent_type", step.SubagentType),
		slog.Int("modified_files", len(step.ModifiedFiles)),
		slog.Int("new_files", len(step.NewFiles)),
		slog.Int("deleted_files", len(step.DeletedFiles)),
		slog.String("shadow_branch", shadowBranchName),
		slog.Bool("branch_created", !branchExisted),
	}
	if step.IsIncremental {
		attrs = append(attrs,
			slog.Bool("is_incremental", true),
			slog.String("incremental_type", step.IncrementalType),
			slog.Int("incremental_sequence", step.IncrementalSequence),
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

// deleteShadowBranch deletes a shadow branch by name.
// Returns nil if the branch doesn't exist (idempotent).
// Uses git CLI instead of go-git's RemoveReference because go-git v5
// doesn't properly persist deletions with packed refs or worktrees.
func deleteShadowBranch(ctx context.Context, _ *git.Repository, branchName string) error {
	err := DeleteBranchCLI(ctx, branchName)
	if err != nil {
		// If the branch doesn't exist, treat as idempotent - not an error condition.
		if errors.Is(err, ErrBranchNotFound) {
			return nil
		}
		return err
	}
	return nil
}
