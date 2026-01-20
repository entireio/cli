package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"

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
		state, err = s.initializeSession(repo, sessionID, agentType)
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

	// Use WriteTemporary to create the checkpoint
	isFirstCheckpointOfSession := state.CheckpointCount == 0
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

	// Track touched files (modified, new, and deleted)
	state.FilesTouched = mergeFilesTouched(state.FilesTouched, ctx.ModifiedFiles, ctx.NewFiles, ctx.DeletedFiles)

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
		state, err = s.initializeSession(repo, ctx.SessionID, agentType)
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
	commitMsg := paths.FormatShadowTaskCommitMessage(
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
