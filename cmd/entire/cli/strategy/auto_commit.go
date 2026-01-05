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
	"sync"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// isNotFoundError checks if an error represents a "not found" condition in go-git.
// This includes entry not found, file not found, directory not found, and object not found.
func isNotFoundError(err error) bool {
	return errors.Is(err, object.ErrEntryNotFound) ||
		errors.Is(err, object.ErrFileNotFound) ||
		errors.Is(err, object.ErrDirectoryNotFound) ||
		errors.Is(err, plumbing.ErrObjectNotFound) ||
		errors.Is(err, plumbing.ErrReferenceNotFound)
}

// commitOrHead attempts to create a commit. If the commit would be empty (files already
// committed), it returns HEAD hash instead. This handles the case where files were
// modified during a session but already committed by the user before the hook runs.
func commitOrHead(repo *git.Repository, worktree *git.Worktree, msg string, author *object.Signature) (plumbing.Hash, error) {
	commitHash, err := worktree.Commit(msg, &git.CommitOptions{Author: author})
	if errors.Is(err, git.ErrEmptyCommit) {
		fmt.Fprintf(os.Stderr, "No changes to commit (files already committed)\n")
		head, err := repo.Head()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get HEAD: %w", err)
		}
		return head.Hash(), nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to commit: %w", err)
	}
	return commitHash, nil
}

// AutoCommitStrategy implements the auto-commit strategy:
// - Code changes are committed to the active branch (like commit strategy)
// - Session logs are committed to a shadow branch (like manual-commit strategy)
// - Code commits can reference the shadow branch via trailers
type AutoCommitStrategy struct {
	// checkpointStore manages checkpoint data on entire/sessions branch
	checkpointStore *checkpoint.GitStore
	// checkpointStoreOnce ensures thread-safe lazy initialization
	checkpointStoreOnce sync.Once
	// checkpointStoreErr captures any error during initialization
	checkpointStoreErr error
}

// getCheckpointStore returns the checkpoint store, initializing it lazily if needed.
// Thread-safe via sync.Once.
func (s *AutoCommitStrategy) getCheckpointStore() (*checkpoint.GitStore, error) {
	s.checkpointStoreOnce.Do(func() {
		repo, err := OpenRepository()
		if err != nil {
			s.checkpointStoreErr = fmt.Errorf("failed to open repository: %w", err)
			return
		}
		s.checkpointStore = checkpoint.NewGitStore(repo)
	})
	return s.checkpointStore, s.checkpointStoreErr
}

// NewAutoCommitStrategy creates a new AutoCommitStrategy instance
//

func NewAutoCommitStrategy() Strategy {
	return &AutoCommitStrategy{}
}

// NewDualStrategy creates a new auto-commit strategy instance.
// This legacy constructor delegates to NewAutoCommitStrategy.
//

func NewDualStrategy() Strategy {
	return NewAutoCommitStrategy()
}

func (s *AutoCommitStrategy) Name() string {
	return StrategyNameAutoCommit
}

func (s *AutoCommitStrategy) Description() string {
	return "Auto-commits code to active branch with metadata on entire/sessions"
}

// AllowsMainBranch returns false because auto-commit strategy creates commits on the
// working branch, which would pollute main branch history.
func (s *AutoCommitStrategy) AllowsMainBranch() bool {
	return false
}

func (s *AutoCommitStrategy) ValidateRepository() error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	_, err = repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to access worktree: %w", err)
	}

	return nil
}

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the entire/sessions branch alongside the user's push.
// Configuration options (stored in .entire/settings.json under strategy_options.push_sessions):
//   - "auto": always push automatically
//   - "prompt" (default): ask user with option to enable auto
//   - "false"/"off"/"no": never push
func (s *AutoCommitStrategy) PrePush(remote string) error {
	return pushSessionsBranchCommon(remote, paths.MetadataBranchName)
}

func (s *AutoCommitStrategy) SaveChanges(ctx SaveContext) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Generate checkpoint ID for this commit
	checkpointID := paths.GenerateCheckpointID()

	// Step 1: Commit code changes to active branch with checkpoint ID trailer
	// We do code first to avoid orphaned metadata if this step fails.
	// If metadata commit fails after this, the code commit exists but GetRewindPoints
	// already handles missing metadata gracefully (skips commits without metadata).
	_, err = s.commitCodeToActive(repo, ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("failed to commit code to active branch: %w", err)
	}

	// Step 2: Commit metadata to entire/sessions branch using sharded path
	// Path is <checkpointID[:2]>/<checkpointID[2:]>/ for direct lookup
	_, err = s.commitMetadataToMetadataBranch(repo, ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("failed to commit metadata to entire/sessions branch: %w", err)
	}

	// Log checkpoint creation
	logCtx := logging.WithComponent(context.Background(), "checkpoint")
	logging.Info(logCtx, "checkpoint saved",
		slog.String("strategy", "auto-commit"),
		slog.String("checkpoint_type", "session"),
		slog.String("checkpoint_id", checkpointID),
		slog.Int("modified_files", len(ctx.ModifiedFiles)),
		slog.Int("new_files", len(ctx.NewFiles)),
		slog.Int("deleted_files", len(ctx.DeletedFiles)),
	)

	return nil
}

// commitCodeToActive commits code changes to the active branch.
// Adds an Entire-Checkpoint trailer for metadata lookup that survives amend/rebase.
// Returns the commit hash so metadata can be stored on entire/sessions.
func (s *AutoCommitStrategy) commitCodeToActive(repo *git.Repository, ctx SaveContext, checkpointID string) (plumbing.Hash, error) {
	// Check if there are any code changes to commit
	if len(ctx.ModifiedFiles) == 0 && len(ctx.NewFiles) == 0 && len(ctx.DeletedFiles) == 0 {
		fmt.Fprintf(os.Stderr, "No code changes to commit to active branch\n")
		// Return current HEAD hash so metadata can still be stored
		head, err := repo.Head()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get HEAD: %w", err)
		}
		return head.Hash(), nil
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get worktree: %w", err)
	}

	// Stage code changes
	StageFiles(worktree, ctx.ModifiedFiles, ctx.NewFiles, ctx.DeletedFiles, StageForSession)

	// Add checkpoint ID trailer to commit message
	commitMsg := ctx.CommitMessage + "\n\n" + paths.CheckpointTrailerKey + ": " + checkpointID

	author := &object.Signature{
		Name:  ctx.AuthorName,
		Email: ctx.AuthorEmail,
		When:  time.Now(),
	}
	commitHash, err := commitOrHead(repo, worktree, commitMsg, author)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	fmt.Fprintf(os.Stderr, "Committed code changes to active branch (%s)\n", commitHash.String()[:7])
	return commitHash, nil
}

// commitMetadataToMetadataBranch commits session metadata to the entire/sessions branch.
// Metadata is stored at sharded path: <checkpointID[:2]>/<checkpointID[2:]>/
// This allows direct lookup from the checkpoint ID trailer on the code commit.
// Uses checkpoint.WriteCommitted for git operations.
func (s *AutoCommitStrategy) commitMetadataToMetadataBranch(_ *git.Repository, ctx SaveContext, checkpointID string) (plumbing.Hash, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Extract session ID from metadata dir
	sessionID := filepath.Base(ctx.MetadataDir)

	// Write committed checkpoint using the checkpoint store
	err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     StrategyNameAutoCommit, // Use new strategy name
		MetadataDir:  ctx.MetadataDirAbs,     // Copy all files from metadata dir
		AuthorName:   ctx.AuthorName,
		AuthorEmail:  ctx.AuthorEmail,
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to write committed checkpoint: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Committed session metadata to %s (%s)\n", paths.MetadataBranchName, checkpointID)
	return plumbing.ZeroHash, nil // Commit hash not needed by callers
}

func (s *AutoCommitStrategy) GetRewindPoints(limit int) ([]RewindPoint, error) {
	// For auto-commit strategy, rewind points are found by looking for Entire-Checkpoint trailers
	// in the current branch's commit history. The checkpoint ID provides direct lookup
	// to metadata on entire/sessions branch.
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Get metadata branch tree for lookups
	metadataTree, err := GetMetadataBranchTree(repo)
	if err != nil {
		// No metadata branch yet is fine
		return []RewindPoint{}, nil //nolint:nilerr // Expected when no metadata exists
	}

	// Get the main branch commit hash to determine branch-only commits
	mainBranchHash := GetMainBranchHash(repo)

	// Walk current branch history looking for commits with checkpoint trailers
	iter, err := repo.Log(&git.LogOptions{
		From:  head.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %w", err)
	}

	var points []RewindPoint
	count := 0

	err = iter.ForEach(func(c *object.Commit) error {
		if count >= logsOnlyScanLimit || len(points) >= limit {
			return errStop
		}
		count++

		// Check for Entire-Checkpoint trailer
		checkpointID, found := paths.ParseCheckpointTrailer(c.Message)
		if !found {
			return nil
		}

		// Look up metadata from sharded path
		checkpointPath := paths.CheckpointPath(checkpointID)
		metadata, err := ReadCheckpointMetadata(metadataTree, checkpointPath)
		if err != nil {
			// Checkpoint exists in commit but no metadata found - skip this commit
			return nil //nolint:nilerr // Intentional: skip commits without metadata
		}

		message := strings.Split(c.Message, "\n")[0]

		// Determine if this is a full rewind or logs-only
		// Full rewind is allowed if commit is only on this branch (not reachable from main)
		isLogsOnly := false
		if mainBranchHash != plumbing.ZeroHash {
			if isAncestor(repo, c.Hash, mainBranchHash) {
				isLogsOnly = true
			}
		}

		// Build metadata path - for task checkpoints, include the task path
		metadataDir := checkpointPath
		if metadata.IsTask && metadata.ToolUseID != "" {
			metadataDir = checkpointPath + "/tasks/" + metadata.ToolUseID
		}

		points = append(points, RewindPoint{
			ID:               c.Hash.String(),
			Message:          message,
			MetadataDir:      metadataDir,
			Date:             c.Author.When,
			IsLogsOnly:       isLogsOnly,
			CheckpointID:     checkpointID,
			IsTaskCheckpoint: metadata.IsTask,
			ToolUseID:        metadata.ToolUseID,
		})

		return nil
	})

	if err != nil && !errors.Is(err, errStop) {
		return nil, fmt.Errorf("failed to iterate commits: %w", err)
	}

	return points, nil
}

// isAncestor checks if commit is an ancestor of (or equal to) target.
// Returns true if target can reach commit by following parent links.
func isAncestor(repo *git.Repository, commit, target plumbing.Hash) bool {
	if commit == target {
		return true
	}

	// Walk from target backwards to see if we reach commit
	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false
	}

	found := false
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are non-fatal
		count++
		if count > 1000 {
			return errStop // Limit search depth
		}
		if c.Hash == commit {
			found = true
			return errStop
		}
		return nil
	})

	return found
}

// findTaskMetadataPathForCommit looks up the task metadata path for a task checkpoint commit
// by searching the entire/sessions branch commit history for the checkpoint directory.
// Returns ("", nil) if metadata is not found - this is expected for commits without metadata.
func (s *AutoCommitStrategy) findTaskMetadataPathForCommit(repo *git.Repository, commitSHA, toolUseID string) (string, error) {
	// Get the entire/sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		if isNotFoundError(err) {
			return "", nil // No metadata branch yet
		}
		return "", fmt.Errorf("failed to get metadata branch: %w", err)
	}

	// Search commit history for a commit referencing this code commit SHA and tool use ID
	shortSHA := commitSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}

	iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return "", fmt.Errorf("failed to get commit log: %w", err)
	}

	var foundTaskPath string
	err = iter.ForEach(func(commit *object.Commit) error {
		// Check if commit message contains "Commit: <sha>" and the tool use ID
		if strings.Contains(commit.Message, "Commit: "+shortSHA) &&
			strings.Contains(commit.Message, toolUseID) {
			// Parse task metadata trailer
			if taskPath, found := paths.ParseTaskMetadataTrailer(commit.Message); found {
				foundTaskPath = taskPath
				return errStop // Found it
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return "", fmt.Errorf("failed to iterate commits: %w", err)
	}

	return foundTaskPath, nil
}

func (s *AutoCommitStrategy) Rewind(point RewindPoint) error {
	commitHash := plumbing.NewHash(point.ID)
	shortID, err := HardResetWithProtection(commitHash)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("Reset to commit %s\n", shortID)
	fmt.Println()

	return nil
}

func (s *AutoCommitStrategy) CanRewind() (bool, string, error) {
	return checkCanRewind()
}

// PreviewRewind returns what will happen if rewinding to the given point.
// For auto-commit strategy, this returns nil since git reset doesn't delete untracked files.
func (s *AutoCommitStrategy) PreviewRewind(_ RewindPoint) (*RewindPreview, error) {
	// Auto-commit uses git reset --hard which doesn't affect untracked files
	// Return empty preview to indicate no untracked files will be deleted
	return &RewindPreview{}, nil
}

func (s *AutoCommitStrategy) GetSessionLog(checkpointID string) ([]byte, string, error) {
	// Session logs are on the entire/sessions branch in checkpoint directories
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Use checkpoint.ReadCommitted for new format
	result, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if result == nil {
		// Try legacy format directly if not found in sharded format
		return s.getSessionLogLegacy(checkpointID)
	}

	return result.Transcript, result.Metadata.SessionID, nil
}

// getSessionLogLegacy reads session logs from legacy format (cond-xxx/full.jsonl).
func (s *AutoCommitStrategy) getSessionLogLegacy(checkpointID string) ([]byte, string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, "", fmt.Errorf("failed to open git repository: %w", err)
	}
	// Get the entire/sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, "", fmt.Errorf("metadata branch %s not found: %w", paths.MetadataBranchName, err)
	}

	metadataCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, "", fmt.Errorf("failed to get metadata branch commit: %w", err)
	}

	tree, err := metadataCommit.Tree()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get metadata tree: %w", err)
	}

	// Legacy path format
	basePath := checkpointID

	// Get session ID from metadata.json
	var sessionID string
	metadataPath := basePath + "/metadata.json"
	if metaFile, fileErr := tree.File(metadataPath); fileErr == nil {
		if content, contentErr := metaFile.Contents(); contentErr == nil {
			var metadata map[string]interface{}
			if json.Unmarshal([]byte(content), &metadata) == nil {
				if sid, ok := metadata["session_id"].(string); ok {
					sessionID = sid
				}
			}
		}
	}

	// Try current format first, then legacy
	transcriptPath := basePath + "/" + paths.TranscriptFileName
	file, err := tree.File(transcriptPath)
	if err != nil {
		transcriptPath = basePath + "/" + paths.TranscriptFileNameLegacy
		file, err = tree.File(transcriptPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to find session log (tried %s and %s): %w", paths.TranscriptFileName, paths.TranscriptFileNameLegacy, err)
		}
	}

	content, err := file.Contents()
	if err != nil {
		return nil, "", fmt.Errorf("failed to read session log: %w", err)
	}

	return []byte(content), sessionID, nil
}

// EnsureSetup ensures the strategy's required setup is in place.
// For auto-commit strategy:
// - Gitignore metadata/
// - Create orphan entire/sessions branch if it doesn't exist
func (s *AutoCommitStrategy) EnsureSetup() error {
	if err := EnsureMetadataGitignore(); err != nil {
		return err
	}

	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Ensure the entire/sessions orphan branch exists
	if err := EnsureMetadataBranch(repo); err != nil {
		return fmt.Errorf("failed to ensure metadata branch: %w", err)
	}

	return nil
}

// GetSessionInfo returns session information for linking commits.
// For auto-commit strategy, we don't track active sessions - metadata is stored on
// entire/sessions branch when SaveChanges is called. Active branch commits
// are kept clean (no trailers), so this returns ErrNoSession.
// Use ListSessions() or GetSession() to retrieve session info from the metadata branch.
func (s *AutoCommitStrategy) GetSessionInfo() (*SessionInfo, error) {
	// Dual strategy doesn't track active sessions like shadow does.
	// Session metadata is stored on entire/sessions branch and can be
	// retrieved via ListSessions() or GetSession().
	return nil, ErrNoSession
}

// SaveTaskCheckpoint creates a checkpoint commit for a completed task.
// For auto-commit strategy:
// 1. Commit code changes to active branch (no trailers - clean history)
// 2. Commit task metadata to entire/sessions branch with checkpoint format
func (s *AutoCommitStrategy) SaveTaskCheckpoint(ctx TaskCheckpointContext) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Ensure entire/sessions branch exists
	if err := EnsureMetadataBranch(repo); err != nil {
		return fmt.Errorf("failed to ensure metadata branch: %w", err)
	}

	// Generate checkpoint ID for this task checkpoint
	checkpointID := paths.GenerateCheckpointID()

	// Step 1: Commit code changes to active branch with checkpoint ID trailer
	// We do code first to avoid orphaned metadata if this step fails.
	_, err = s.commitTaskCodeToActive(repo, ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("failed to commit task code to active branch: %w", err)
	}

	// Step 2: Commit task metadata to entire/sessions branch at sharded path
	_, err = s.commitTaskMetadataToMetadataBranch(repo, ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("failed to commit task metadata to entire/sessions branch: %w", err)
	}

	// Log task checkpoint creation
	logCtx := logging.WithComponent(context.Background(), "checkpoint")
	attrs := []any{
		slog.String("strategy", "auto-commit"),
		slog.String("checkpoint_type", "task"),
		slog.String("checkpoint_id", checkpointID),
		slog.String("checkpoint_uuid", ctx.CheckpointUUID),
		slog.String("tool_use_id", ctx.ToolUseID),
		slog.String("subagent_type", ctx.SubagentType),
		slog.Int("modified_files", len(ctx.ModifiedFiles)),
		slog.Int("new_files", len(ctx.NewFiles)),
		slog.Int("deleted_files", len(ctx.DeletedFiles)),
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

// commitTaskCodeToActive commits task code changes to the active branch.
// Adds an Entire-Checkpoint trailer for metadata lookup that survives amend/rebase.
// For TaskStart checkpoints, creates an empty marker commit even without file changes.
func (s *AutoCommitStrategy) commitTaskCodeToActive(repo *git.Repository, ctx TaskCheckpointContext, checkpointID string) (plumbing.Hash, error) {
	// For TaskStart, we want to create a marker commit even without file changes
	isTaskStart := ctx.IsIncremental && ctx.IncrementalType == IncrementalTypeTaskStart
	hasFileChanges := len(ctx.ModifiedFiles) > 0 || len(ctx.NewFiles) > 0 || len(ctx.DeletedFiles) > 0

	// If no file changes and not a TaskStart, skip code commit
	if !hasFileChanges && !isTaskStart {
		fmt.Fprintf(os.Stderr, "No code changes to commit for task checkpoint\n")
		// Return current HEAD hash so metadata can still be stored
		head, err := repo.Head()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get HEAD: %w", err)
		}
		return head.Hash(), nil
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get worktree: %w", err)
	}

	// Stage code changes
	StageFiles(worktree, ctx.ModifiedFiles, ctx.NewFiles, ctx.DeletedFiles, StageForTask)

	// Build commit message with checkpoint trailer
	shortToolUseID := ctx.ToolUseID
	if len(shortToolUseID) > 12 {
		shortToolUseID = shortToolUseID[:12]
	}

	var subject string
	if ctx.IsIncremental {
		subject = FormatIncrementalSubject(
			ctx.IncrementalType,
			ctx.SubagentType,
			ctx.TaskDescription,
			ctx.TodoContent,
			ctx.IncrementalSequence,
			shortToolUseID,
		)
	} else {
		subject = FormatSubagentEndMessage(ctx.SubagentType, ctx.TaskDescription, shortToolUseID)
	}

	// Add checkpoint ID trailer to commit message
	commitMsg := subject + "\n\n" + paths.CheckpointTrailerKey + ": " + checkpointID

	author := &object.Signature{
		Name:  ctx.AuthorName,
		Email: ctx.AuthorEmail,
		When:  time.Now(),
	}

	var commitHash plumbing.Hash
	if isTaskStart {
		// For TaskStart, allow empty commits (marker commits)
		commitHash, err = worktree.Commit(commitMsg, &git.CommitOptions{
			Author:            author,
			AllowEmptyCommits: true,
		})
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to create TaskStart marker commit: %w", err)
		}
	} else {
		commitHash, err = commitOrHead(repo, worktree, commitMsg, author)
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}

	if ctx.IsIncremental {
		fmt.Fprintf(os.Stderr, "Committed incremental checkpoint #%d to active branch (%s)\n", ctx.IncrementalSequence, commitHash.String()[:7])
	} else {
		fmt.Fprintf(os.Stderr, "Committed task checkpoint to active branch (%s)\n", commitHash.String()[:7])
	}
	return commitHash, nil
}

// commitTaskMetadataToMetadataBranch commits task metadata to the entire/sessions branch.
// Uses sharded path: <checkpointID[:2]>/<checkpointID[2:]>/tasks/<tool-use-id>/
// Returns the metadata commit hash.
// When IsIncremental is true, only writes the incremental checkpoint file, skipping transcripts.
// Uses checkpoint.WriteCommitted for git operations.
func (s *AutoCommitStrategy) commitTaskMetadataToMetadataBranch(_ *git.Repository, ctx TaskCheckpointContext, checkpointID string) (plumbing.Hash, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Format commit subject line for better git log readability
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

	// Write committed checkpoint using the checkpoint store
	err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID:           checkpointID,
		SessionID:              ctx.SessionID,
		Strategy:               StrategyNameAutoCommit,
		IsTask:                 true,
		ToolUseID:              ctx.ToolUseID,
		AgentID:                ctx.AgentID,
		CheckpointUUID:         ctx.CheckpointUUID,
		TranscriptPath:         ctx.TranscriptPath,
		SubagentTranscriptPath: ctx.SubagentTranscriptPath,
		IsIncremental:          ctx.IsIncremental,
		IncrementalSequence:    ctx.IncrementalSequence,
		IncrementalType:        ctx.IncrementalType,
		IncrementalData:        ctx.IncrementalData,
		CommitSubject:          messageSubject,
		AuthorName:             ctx.AuthorName,
		AuthorEmail:            ctx.AuthorEmail,
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to write task checkpoint: %w", err)
	}

	if ctx.IsIncremental {
		fmt.Fprintf(os.Stderr, "Committed incremental checkpoint metadata to %s (%s)\n", paths.MetadataBranchName, checkpointID)
	} else {
		fmt.Fprintf(os.Stderr, "Committed task metadata to %s (%s)\n", paths.MetadataBranchName, checkpointID)
	}
	return plumbing.ZeroHash, nil // Commit hash not needed by callers
}

// GetTaskCheckpoint returns the task checkpoint for a given rewind point.
// For auto-commit strategy, checkpoints are stored on the entire/sessions branch in checkpoint directories.
// Returns ErrNotTaskCheckpoint if the point is not a task checkpoint.
func (s *AutoCommitStrategy) GetTaskCheckpoint(point RewindPoint) (*TaskCheckpoint, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	// Get the entire/sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("metadata branch %s not found: %w", paths.MetadataBranchName, err)
	}

	metadataCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch commit: %w", err)
	}

	tree, err := metadataCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata tree: %w", err)
	}

	// Find checkpoint using the metadata path from rewind point
	// MetadataDir for auto-commit task checkpoints is: cond-YYYYMMDD-HHMMSS-XXXXXXXX/tasks/<tool-use-id>
	checkpointPath := point.MetadataDir + "/checkpoint.json"
	file, err := tree.File(checkpointPath)
	if err != nil {
		// Try finding via commit SHA lookup
		taskCheckpointPath, findErr := s.findTaskCheckpointPath(repo, point.ID, point.ToolUseID)
		if findErr != nil {
			return nil, fmt.Errorf("failed to find checkpoint at %s: %w", checkpointPath, err)
		}
		file, err = tree.File(taskCheckpointPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find checkpoint at %s: %w", taskCheckpointPath, err)
		}
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var checkpoint TaskCheckpoint
	if err := json.Unmarshal([]byte(content), &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// GetTaskCheckpointTranscript returns the session transcript for a task checkpoint.
// For auto-commit strategy, transcripts are stored on the entire/sessions branch in checkpoint directories.
// Returns ErrNotTaskCheckpoint if the point is not a task checkpoint.
func (s *AutoCommitStrategy) GetTaskCheckpointTranscript(point RewindPoint) ([]byte, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	// Get the entire/sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("metadata branch %s not found: %w", paths.MetadataBranchName, err)
	}

	metadataCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch commit: %w", err)
	}

	tree, err := metadataCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata tree: %w", err)
	}

	// MetadataDir for auto-commit task checkpoints is: cond-YYYYMMDD-HHMMSS-XXXXXXXX/tasks/<tool-use-id>
	// Session transcript is at: cond-YYYYMMDD-HHMMSS-XXXXXXXX/<TranscriptFileName>
	// Extract the checkpoint path by removing "/tasks/<tool-use-id>"
	metadataDir := point.MetadataDir
	if idx := strings.Index(metadataDir, "/tasks/"); idx > 0 {
		checkpointPath := metadataDir[:idx]
		transcriptPath := checkpointPath + "/" + paths.TranscriptFileName
		file, err := tree.File(transcriptPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find transcript at %s: %w", transcriptPath, err)
		}
		content, err := file.Contents()
		if err != nil {
			return nil, fmt.Errorf("failed to read transcript: %w", err)
		}
		return []byte(content), nil
	}

	return nil, fmt.Errorf("invalid metadata path format: %s", metadataDir)
}

// findTaskCheckpointPath finds the full path to a task checkpoint on the entire/sessions branch.
// Searches checkpoint directories for the task checkpoint matching the commit SHA and tool use ID.
func (s *AutoCommitStrategy) findTaskCheckpointPath(repo *git.Repository, commitSHA, toolUseID string) (string, error) {
	// Use findTaskMetadataPathForCommit which searches commit history
	taskPath, err := s.findTaskMetadataPathForCommit(repo, commitSHA, toolUseID)
	if err != nil {
		return "", err
	}
	if taskPath == "" {
		return "", errors.New("task checkpoint not found")
	}
	// taskPath is like: cond-YYYYMMDD-HHMMSS-XXXXXXXX/tasks/<tool-use-id>/checkpoints/001-<tool-use-id>.json
	// We need: cond-YYYYMMDD-HHMMSS-XXXXXXXX/tasks/<tool-use-id>/checkpoint.json
	if idx := strings.Index(taskPath, "/checkpoints/"); idx > 0 {
		return taskPath[:idx] + "/checkpoint.json", nil
	}
	return taskPath + "/checkpoint.json", nil
}

// GetMetadataRef returns a reference to the metadata for the given checkpoint.
// For auto-commit strategy, returns the checkpoint path on entire/sessions branch.
func (s *AutoCommitStrategy) GetMetadataRef(checkpoint Checkpoint) string {
	if checkpoint.CheckpointID == "" {
		return ""
	}
	return paths.MetadataBranchName + ":" + paths.CheckpointPath(checkpoint.CheckpointID)
}

// GetSessionMetadataRef returns a reference to the most recent metadata for a session.
func (s *AutoCommitStrategy) GetSessionMetadataRef(sessionID string) string {
	session, err := GetSession(sessionID)
	if err != nil || len(session.Checkpoints) == 0 {
		return ""
	}
	// Checkpoints are ordered with most recent first
	return s.GetMetadataRef(session.Checkpoints[0])
}

// GetSessionContext returns the context.md content for a session.
// For auto-commit strategy, reads from the entire/sessions branch using the checkpoint store.
func (s *AutoCommitStrategy) GetSessionContext(sessionID string) string {
	session, err := GetSession(sessionID)
	if err != nil || len(session.Checkpoints) == 0 {
		return ""
	}

	// Get the most recent checkpoint
	cp := session.Checkpoints[0]
	if cp.CheckpointID == "" {
		return ""
	}

	store, err := s.getCheckpointStore()
	if err != nil {
		return ""
	}

	result, err := store.ReadCommitted(context.Background(), cp.CheckpointID)
	if err != nil || result == nil {
		return ""
	}

	return result.Context
}

// GetCheckpointLog returns the session transcript for a specific checkpoint.
// For auto-commit strategy, looks up checkpoint by ID on the entire/sessions branch using the checkpoint store.
func (s *AutoCommitStrategy) GetCheckpointLog(cp Checkpoint) ([]byte, error) {
	if cp.CheckpointID == "" {
		return nil, ErrNoMetadata
	}

	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	result, err := store.ReadCommitted(context.Background(), cp.CheckpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if result == nil {
		return nil, ErrNoMetadata
	}

	return result.Transcript, nil
}

// InitializeSession creates session state for a new session.
// This is called during UserPromptSubmit hook to set up tracking for the session.
// For auto-commit strategy, this creates a SessionState file in .git/entire-sessions/
// to track CondensedTranscriptLines (transcript offset) across checkpoints.
func (s *AutoCommitStrategy) InitializeSession(sessionID string) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get current HEAD commit to track as base
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	baseCommit := head.Hash().String()[:7]

	// Check if session state already exists (e.g., session resuming)
	existing, err := LoadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to check existing session state: %w", err)
	}
	if existing != nil {
		// Session already initialized, nothing to do
		return nil
	}

	// Create new session state
	state := &SessionState{
		SessionID:                sessionID,
		BaseCommit:               baseCommit,
		StartedAt:                time.Now(),
		CheckpointCount:          0,
		CondensedTranscriptLines: 0, // Start from beginning of transcript
		FilesTouched:             []string{},
	}

	if err := SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	return nil
}

// ListOrphanedItems returns orphaned items created by the auto-commit strategy.
// For auto-commit, checkpoints are orphaned when no commit has an Entire-Checkpoint
// trailer referencing them (e.g., after rebasing or squashing).
func (s *AutoCommitStrategy) ListOrphanedItems() ([]CleanupItem, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	// Get checkpoint store (lazily initialized)
	cpStore, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Get all checkpoints from entire/sessions branch
	checkpoints, err := cpStore.ListCommitted(context.Background())
	if err != nil {
		return []CleanupItem{}, nil //nolint:nilerr // No checkpoints is not an error for cleanup
	}

	if len(checkpoints) == 0 {
		return []CleanupItem{}, nil
	}

	// Filter to only auto-commit checkpoints (identified by strategy in metadata)
	autoCommitCheckpoints := make(map[string]bool)
	for _, cp := range checkpoints {
		result, readErr := cpStore.ReadCommitted(context.Background(), cp.CheckpointID)
		if readErr != nil || result == nil {
			continue
		}
		// Only consider checkpoints created by this strategy
		if result.Metadata.Strategy == StrategyNameAutoCommit || result.Metadata.Strategy == StrategyNameDual {
			autoCommitCheckpoints[cp.CheckpointID] = true
		}
	}

	if len(autoCommitCheckpoints) == 0 {
		return []CleanupItem{}, nil
	}

	// Find checkpoint IDs referenced in commits
	referencedCheckpoints := s.findReferencedCheckpoints(repo)

	// Find orphaned checkpoints
	var items []CleanupItem
	for checkpointID := range autoCommitCheckpoints {
		if !referencedCheckpoints[checkpointID] {
			items = append(items, CleanupItem{
				Type:   CleanupTypeCheckpoint,
				ID:     checkpointID,
				Reason: "no commit references this checkpoint",
			})
		}
	}

	return items, nil
}

// findReferencedCheckpoints scans commits for Entire-Checkpoint trailers.
func (s *AutoCommitStrategy) findReferencedCheckpoints(repo *git.Repository) map[string]bool {
	referenced := make(map[string]bool)

	refs, err := repo.References()
	if err != nil {
		return referenced
	}

	visited := make(map[plumbing.Hash]bool)

	_ = refs.ForEach(func(ref *plumbing.Reference) error { //nolint:errcheck // Best effort
		if !ref.Name().IsBranch() {
			return nil
		}
		// Skip entire/* branches
		branchName := strings.TrimPrefix(ref.Name().String(), "refs/heads/")
		if strings.HasPrefix(branchName, "entire/") {
			return nil
		}

		iter, iterErr := repo.Log(&git.LogOptions{From: ref.Hash()})
		if iterErr != nil {
			return nil //nolint:nilerr // Best effort
		}

		count := 0
		_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best effort
			count++
			if count > 1000 {
				return errors.New("limit reached")
			}
			if visited[c.Hash] {
				return nil
			}
			visited[c.Hash] = true

			if checkpointID, found := paths.ParseCheckpointTrailer(c.Message); found {
				referenced[checkpointID] = true
			}
			return nil
		})
		return nil
	})

	return referenced
}

//nolint:gochecknoinits // Standard pattern for strategy registration
func init() {
	// Register auto-commit as the primary strategy name
	Register(StrategyNameAutoCommit, NewAutoCommitStrategy)
}
