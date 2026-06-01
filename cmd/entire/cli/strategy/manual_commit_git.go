package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/perf"

	"github.com/go-git/go-git/v6"
)

// SaveStep saves a checkpoint to the shadow branch.
// Uses checkpoint.GitStore.WriteTemporary for git operations.
func (s *ManualCommitStrategy) SaveStep(ctx context.Context, step StepContext) error {
	_, openRepoSpan := perf.Start(ctx, "open_repository")
	repo, err := OpenRepository(ctx)
	if err != nil {
		openRepoSpan.RecordError(err)
		openRepoSpan.End()
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()
	openRepoSpan.End()

	sessionID := filepath.Base(step.MetadataDir)

	// Initialize the session if no state exists yet. Done outside
	// MutateSessionState because the helper bails with ErrStateNotFound on
	// missing state — initialization establishes the file the helper will
	// then mutate under lock.
	if err := s.ensureSessionInitialized(ctx, repo, sessionID, step.AgentType); err != nil {
		return err
	}

	mutErr := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		_, migrateSpan := perf.Start(ctx, "migrate_shadow_branch")
		if _, _, err := s.migrateShadowBranchIfNeeded(ctx, repo, state); err != nil {
			migrateSpan.RecordError(err)
			migrateSpan.End()
			return fmt.Errorf("failed to check/migrate shadow branch: %w", err)
		}
		migrateSpan.End()

		store := s.getCheckpointStore(repo)

		shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		branchExisted := store.ShadowBranchExists(state.BaseCommit, state.WorktreeID)

		var promptAttr PromptAttribution
		if state.PendingPromptAttribution != nil {
			promptAttr = *state.PendingPromptAttribution
			state.PendingPromptAttribution = nil
		} else {
			promptAttr = PromptAttribution{CheckpointNumber: state.StepCount + 1}
		}

		attrLogCtx := logging.WithComponent(ctx, "attribution")
		logging.Debug(attrLogCtx, "prompt attribution at checkpoint save",
			slog.Int("checkpoint_number", promptAttr.CheckpointNumber),
			slog.Int("user_added", promptAttr.UserLinesAdded),
			slog.Int("user_removed", promptAttr.UserLinesRemoved),
			slog.Int("agent_added", promptAttr.AgentLinesAdded),
			slog.Int("agent_removed", promptAttr.AgentLinesRemoved),
			slog.String("session_id", sessionID))

		_, writeCheckpointSpan := perf.Start(ctx, "write_temporary_checkpoint")
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
		writeCheckpointSpan.RecordError(err)
		writeCheckpointSpan.End()
		if err != nil {
			return fmt.Errorf("failed to write temporary checkpoint: %w", err)
		}

		if result.Skipped {
			logCtx := logging.WithComponent(ctx, "checkpoint")
			logging.Info(logCtx, "checkpoint skipped (no changes)",
				slog.String("strategy", "manual-commit"),
				slog.String("checkpoint_type", "session"),
				slog.Int("checkpoint_count", state.StepCount),
				slog.String("shadow_branch", shadowBranchName),
			)
			return ErrMutationSkip
		}

		// LastCheckpointID is intentionally NOT cleared here. It is set during
		// condensation and used by handleAmendCommitMsg to restore checkpoint
		// trailers on amend operations.
		state.StepCount++
		state.PromptAttributions = append(state.PromptAttributions, promptAttr)
		state.FilesTouched = mergeFilesTouched(state.FilesTouched, step.ModifiedFiles, step.NewFiles, step.DeletedFiles)
		if state.StepCount == 1 {
			state.TranscriptIdentifierAtStart = step.StepTranscriptIdentifier
		}
		if step.TokenUsage != nil {
			state.TokenUsage = accumulateTokenUsage(state.TokenUsage, step.TokenUsage)
		}

		if !branchExisted {
			logging.Info(logging.WithComponent(ctx, "checkpoint"), "created shadow branch and committed changes",
				slog.String("shadow_branch", shadowBranchName))
		} else {
			logging.Info(logging.WithComponent(ctx, "checkpoint"), "committed changes to shadow branch",
				slog.String("shadow_branch", shadowBranchName))
		}

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
	})
	if errors.Is(mutErr, ErrStateNotFound) {
		return nil
	}
	return mutErr
}

// ensureSessionInitialized creates the session state file if it doesn't yet
// exist (or has empty BaseCommit). Idempotent: the existence check and the
// create both happen inside initializeSession's session gate so a concurrent
// turn-start hook can't slip a richer state in between, only to have it
// overwritten with blank fields.
func (s *ManualCommitStrategy) ensureSessionInitialized(ctx context.Context, repo *git.Repository, sessionID string, agentTypeHint types.AgentType) error {
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	agentType := resolveAgentType(agentTypeHint, state)
	if err := s.initializeSession(ctx, repo, sessionID, agentType, "", "", ""); err != nil {
		return fmt.Errorf("failed to initialize session: %w", err)
	}
	return nil
}

// SaveTaskStep saves a task step checkpoint to the shadow branch.
// Uses checkpoint.GitStore.WriteTemporaryTask for git operations.
func (s *ManualCommitStrategy) SaveTaskStep(ctx context.Context, step TaskStepContext) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	if err := s.ensureSessionInitialized(ctx, repo, step.SessionID, step.AgentType); err != nil {
		return err
	}

	mutErr := MutateSessionState(ctx, step.SessionID, func(state *SessionState) error {
		if _, _, err := s.migrateShadowBranchIfNeeded(ctx, repo, state); err != nil {
			return fmt.Errorf("failed to check/migrate shadow branch: %w", err)
		}

		store := s.getCheckpointStore(repo)

		shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		branchExisted := store.ShadowBranchExists(state.BaseCommit, state.WorktreeID)

		sessionMetadataDir := paths.SessionMetadataDirFromSessionID(step.SessionID)
		taskMetadataDir := TaskMetadataDir(sessionMetadataDir, step.ToolUseID)

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

		if _, err := store.WriteTemporaryTask(ctx, checkpoint.WriteTemporaryTaskOptions{
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
		}); err != nil {
			return fmt.Errorf("failed to write task checkpoint: %w", err)
		}

		state.FilesTouched = mergeFilesTouched(state.FilesTouched, step.ModifiedFiles, step.NewFiles, step.DeletedFiles)

		if !branchExisted {
			logging.Info(logging.WithComponent(ctx, "checkpoint"), "created shadow branch and committed task checkpoint",
				slog.String("shadow_branch", shadowBranchName))
		} else {
			logging.Info(logging.WithComponent(ctx, "checkpoint"), "committed task checkpoint to shadow branch",
				slog.String("shadow_branch", shadowBranchName))
		}

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
	})
	if errors.Is(mutErr, ErrStateNotFound) {
		return nil
	}
	return mutErr
}

// mergeFilesTouched merges multiple file lists into existing touched files, deduplicating.
// All paths are normalized to forward slashes for platform-agnostic storage.
func mergeFilesTouched(existing []string, fileLists ...[]string) []string {
	seen := make(map[string]bool)
	for _, f := range existing {
		seen[filepath.ToSlash(f)] = true
	}

	for _, list := range fileLists {
		for _, f := range list {
			seen[filepath.ToSlash(f)] = true
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
