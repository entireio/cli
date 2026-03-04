package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/spf13/cobra"
)

func newResumeCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "resume <branch>",
		Short: "Switch to a branch and resume its session",
		Long: `Switch to a local branch and resume the agent session from its last commit.

This command:
1. Checks out the specified branch
2. Finds the session ID from commits unique to this branch (not on main)
3. Restores the session log if it doesn't exist locally
4. Shows the command to resume the session

If the branch doesn't exist locally but exists on origin, you'll be prompted
to fetch it.

If newer commits without checkpoints exist on the branch (e.g., after merging main
or cherry-picking from elsewhere), this operation will reset your Git status to the
most recent commit with a checkpoint.  You'll be prompted to confirm resuming in this case.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			return runResume(cmd.Context(), args[0], force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Resume from older checkpoint without confirmation")

	return cmd
}

func runResume(ctx context.Context, branchName string, force bool) error {
	// Check if we're already on this branch
	currentBranch, err := GetCurrentBranch(ctx)
	if err == nil && currentBranch == branchName {
		// Already on the branch, skip checkout
		return resumeFromCurrentBranch(ctx, branchName, force)
	}

	// Check if branch exists locally
	exists, err := BranchExistsLocally(ctx, branchName)
	if err != nil {
		return fmt.Errorf("failed to check branch: %w", err)
	}

	if !exists {
		// Branch doesn't exist locally, check if it exists on remote
		remoteExists, err := BranchExistsOnRemote(ctx, branchName)
		if err != nil {
			return fmt.Errorf("failed to check remote branch: %w", err)
		}

		if !remoteExists {
			return fmt.Errorf("branch '%s' not found locally or on origin", branchName)
		}

		// Ask user if they want to fetch from remote
		shouldFetch, err := promptFetchFromRemote(branchName)
		if err != nil {
			return err
		}
		if !shouldFetch {
			return nil
		}

		// Fetch and checkout the remote branch
		fmt.Fprintf(os.Stderr, "Fetching branch '%s' from origin...\n", branchName)
		if err := FetchAndCheckoutRemoteBranch(ctx, branchName); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to checkout branch: %v\n", err)
			return NewSilentError(errors.New("failed to checkout branch"))
		}
		fmt.Fprintf(os.Stderr, "Switched to branch '%s'\n", branchName)
	} else {
		// Branch exists locally, check for uncommitted changes before checkout
		hasChanges, err := HasUncommittedChanges(ctx)
		if err != nil {
			return fmt.Errorf("failed to check for uncommitted changes: %w", err)
		}
		if hasChanges {
			return errors.New("you have uncommitted changes. Please commit or stash them first")
		}

		// Checkout the branch
		if err := CheckoutBranch(ctx, branchName); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to checkout branch: %v\n", err)
			return NewSilentError(errors.New("failed to checkout branch"))
		}
		fmt.Fprintf(os.Stderr, "Switched to branch '%s'\n", branchName)
	}

	return resumeFromCurrentBranch(ctx, branchName, force)
}

func resumeFromCurrentBranch(ctx context.Context, branchName string, force bool) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Find a commit with an Entire-Checkpoint trailer, looking at branch-only commits
	result, err := findBranchCheckpoints(repo, branchName)
	if err != nil {
		return err
	}
	if len(result.checkpointIDs) == 0 {
		fmt.Fprintf(os.Stderr, "No Entire checkpoint found on branch '%s'\n", branchName)
		return nil
	}

	// If there are newer commits without checkpoints, ask for confirmation.
	// Merge commits (e.g., from merging main) don't count as "work" and are skipped silently.
	if result.newerCommitsExist && !force {
		fmt.Fprintf(os.Stderr, "Found checkpoint in an older commit.\n")
		fmt.Fprintf(os.Stderr, "There are %d newer commit(s) on this branch without checkpoints.\n", result.newerCommitCount)
		fmt.Fprintf(os.Stderr, "Checkpoint from: %s %s\n\n", result.commitHash[:7], firstLine(result.commitMessage))

		shouldResume, err := promptResumeFromOlderCheckpoint()
		if err != nil {
			return err
		}
		if !shouldResume {
			fmt.Fprintf(os.Stderr, "Resume cancelled.\n")
			return nil
		}
	}

	// Multiple checkpoints (squash merge): restore all sessions
	if len(result.checkpointIDs) > 1 {
		return resumeMultipleCheckpoints(ctx, repo, result.checkpointIDs, force)
	}

	// Single checkpoint: existing behavior (unchanged)
	checkpointID := result.checkpointIDs[0]

	// Get metadata branch tree for lookups
	metadataTree, err := strategy.GetMetadataBranchTree(repo)
	if err != nil {
		// No local metadata branch, check if remote has it
		return checkRemoteMetadata(ctx, repo, checkpointID)
	}

	// Look up metadata from sharded path
	metadata, err := strategy.ReadCheckpointMetadata(metadataTree, checkpointID.Path())
	if err != nil {
		// Checkpoint exists in commit but no local metadata - check remote
		return checkRemoteMetadata(ctx, repo, checkpointID)
	}

	return resumeSession(ctx, metadata.SessionID, checkpointID, force)
}

// resumeMultipleCheckpoints restores sessions from multiple checkpoint IDs.
// This handles squash merge commits where multiple Entire-Checkpoint trailers
// are present in a single commit message. Each checkpoint is looked up independently
// and missing metadata is skipped (best-effort).
func resumeMultipleCheckpoints(ctx context.Context, repo *git.Repository, checkpointIDs []id.CheckpointID, force bool) error {
	logCtx := logging.WithComponent(ctx, "resume")

	// Get metadata branch tree (try local first, then remote)
	metadataTree, err := strategy.GetMetadataBranchTree(repo)
	if err != nil {
		// Try fetching from remote
		logging.Debug(logCtx, "metadata branch not available locally, trying remote")
		if fetchErr := FetchMetadataBranch(ctx); fetchErr != nil {
			// If fetch also fails, try remote tree directly
			remoteTree, remoteErr := strategy.GetRemoteMetadataBranchTree(repo)
			if remoteErr != nil {
				fmt.Fprintf(os.Stderr, "Checkpoint metadata not available locally or on remote\n")
				return nil //nolint:nilerr // Informational message, not a fatal error
			}
			metadataTree = remoteTree
		} else {
			metadataTree, err = strategy.GetMetadataBranchTree(repo)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Checkpoint metadata not available after fetch\n")
				return nil //nolint:nilerr // Informational message, not a fatal error
			}
		}
	}

	// Read metadata for all checkpoints and sort by CreatedAt ascending
	// (oldest first → newest writes last and wins on disk)
	checkpoints := collectCheckpointsByAge(metadataTree, checkpointIDs)

	// Iterate sorted checkpoints and restore
	strat := GetStrategy(ctx)
	var allSessions []strategy.RestoredSession

	for _, cp := range checkpoints {
		point := strategy.RewindPoint{
			IsLogsOnly:   true,
			CheckpointID: cp.CheckpointID,
			Agent:        cp.Agent,
		}

		sessions, restoreErr := strat.RestoreLogsOnly(ctx, point, force)
		if restoreErr != nil {
			logging.Debug(logCtx, "skipping checkpoint: restore failed",
				slog.String("checkpoint_id", cp.CheckpointID.String()),
				slog.String("error", restoreErr.Error()),
			)
			continue
		}
		if len(sessions) == 0 {
			logging.Debug(logCtx, "skipping checkpoint: no sessions restored",
				slog.String("checkpoint_id", cp.CheckpointID.String()),
			)
			continue
		}

		allSessions = deduplicateSessions(allSessions, sessions)
	}

	if len(allSessions) == 0 {
		fmt.Fprintf(os.Stderr, "No session metadata found for checkpoints in this commit\n")
		return nil
	}

	logging.Debug(logCtx, "resume multiple checkpoints completed",
		slog.Int("checkpoint_count", len(checkpointIDs)),
		slog.Int("session_count", len(allSessions)),
	)

	return displayRestoredSessions(allSessions)
}

// collectCheckpointsByAge reads metadata for each checkpoint ID from the tree,
// skips any that can't be read, and returns them sorted by CreatedAt ascending.
// Sorting ensures the newest checkpoint is restored last and wins on disk,
// regardless of trailer order in the commit message.
func collectCheckpointsByAge(tree *object.Tree, checkpointIDs []id.CheckpointID) []*strategy.CheckpointInfo {
	var checkpoints []*strategy.CheckpointInfo
	for _, cpID := range checkpointIDs {
		metadata, err := strategy.ReadCheckpointMetadata(tree, cpID.Path())
		if err != nil {
			continue
		}
		checkpoints = append(checkpoints, metadata)
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.Before(checkpoints[j].CreatedAt)
	})
	return checkpoints
}

// deduplicateSessions merges new sessions into existing, keeping the one with the latest
// CreatedAt when a SessionID appears more than once. This handles squash merges where the
// same session may be referenced by multiple checkpoints.
func deduplicateSessions(existing, incoming []strategy.RestoredSession) []strategy.RestoredSession {
	type entry struct {
		index     int
		createdAt time.Time
	}

	seen := make(map[string]entry, len(existing))
	for i, s := range existing {
		seen[s.SessionID] = entry{index: i, createdAt: s.CreatedAt}
	}

	for _, sess := range incoming {
		if prev, exists := seen[sess.SessionID]; exists {
			// Keep the one with the later CreatedAt (more complete transcript)
			if sess.CreatedAt.After(prev.createdAt) {
				existing[prev.index] = sess
				seen[sess.SessionID] = entry{index: prev.index, createdAt: sess.CreatedAt}
			}
		} else {
			seen[sess.SessionID] = entry{index: len(existing), createdAt: sess.CreatedAt}
			existing = append(existing, sess)
		}
	}

	return existing
}

// branchCheckpointsResult contains the result of searching for checkpoints on a branch.
type branchCheckpointsResult struct {
	checkpointIDs     []id.CheckpointID
	commitHash        string
	commitMessage     string
	newerCommitsExist bool // true if there are branch-only commits (not merge commits) without checkpoints
	newerCommitCount  int  // count of branch-only commits without checkpoints
}

// findBranchCheckpoints finds the most recent commit with an Entire-Checkpoint trailer
// among commits that are unique to this branch (not reachable from the default branch).
// This handles the case where main has been merged into the feature branch.
func findBranchCheckpoints(repo *git.Repository, branchName string) (*branchCheckpointsResult, error) {
	result := &branchCheckpointsResult{}

	// Get HEAD commit
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD commit: %w", err)
	}

	// First, check if HEAD itself has a checkpoint (most common case)
	if cpIDs := trailers.ParseAllCheckpoints(headCommit.Message); len(cpIDs) > 0 {
		result.checkpointIDs = cpIDs
		result.commitHash = head.Hash().String()
		result.commitMessage = headCommit.Message
		result.newerCommitsExist = false
		return result, nil
	}

	// HEAD doesn't have a checkpoint - find branch-only commits
	// Get the default branch name
	defaultBranch := getDefaultBranchFromRemote(repo)
	if defaultBranch == "" {
		// Fallback: try common names
		for _, name := range []string{"main", "master"} {
			if _, err := repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
				defaultBranch = name
				break
			}
		}
	}

	// If we can't find a default branch, or we're on it, just walk all commits
	if defaultBranch == "" || defaultBranch == branchName {
		return findCheckpointInHistory(headCommit, nil), nil
	}

	// Get the default branch reference
	defaultRef, err := repo.Reference(plumbing.NewBranchReferenceName(defaultBranch), true)
	if err != nil {
		// Default branch doesn't exist locally, fall back to walking all commits
		return findCheckpointInHistory(headCommit, nil), nil //nolint:nilerr // Intentional fallback
	}

	defaultCommit, err := repo.CommitObject(defaultRef.Hash())
	if err != nil {
		// Can't get default commit, fall back to walking all commits
		return findCheckpointInHistory(headCommit, nil), nil //nolint:nilerr // Intentional fallback
	}

	// Find merge base
	mergeBase, err := headCommit.MergeBase(defaultCommit)
	if err != nil || len(mergeBase) == 0 {
		// No common ancestor, fall back to walking all commits
		return findCheckpointInHistory(headCommit, nil), nil //nolint:nilerr // Intentional fallback
	}

	// Walk from HEAD to merge base, looking for checkpoint
	return findCheckpointInHistory(headCommit, &mergeBase[0].Hash), nil
}

// findCheckpointInHistory walks commit history from start looking for a checkpoint trailer.
// If stopAt is provided, stops when reaching that commit (exclusive).
// Returns the first checkpoint found and info about commits between HEAD and the checkpoint.
// It distinguishes between merge commits (bringing in other branches) and regular commits
// (actual branch work) to avoid false warnings after merging main.
func findCheckpointInHistory(start *object.Commit, stopAt *plumbing.Hash) *branchCheckpointsResult {
	result := &branchCheckpointsResult{}
	branchWorkCommits := 0 // Regular commits without checkpoints (actual work)
	const maxCommits = 100 // Limit search depth
	totalChecked := 0

	current := start
	for current != nil && totalChecked < maxCommits {
		// Stop if we've reached the boundary
		if stopAt != nil && current.Hash == *stopAt {
			break
		}

		// Check for checkpoint trailer
		if cpIDs := trailers.ParseAllCheckpoints(current.Message); len(cpIDs) > 0 {
			result.checkpointIDs = cpIDs
			result.commitHash = current.Hash.String()
			result.commitMessage = current.Message
			// Only warn about branch work commits, not merge commits
			result.newerCommitsExist = branchWorkCommits > 0
			result.newerCommitCount = branchWorkCommits
			return result
		}

		// Only count regular commits (not merge commits) as "branch work"
		if current.NumParents() <= 1 {
			branchWorkCommits++
		}

		totalChecked++

		// Move to parent (first parent for merge commits - follows the main line)
		if current.NumParents() == 0 {
			break
		}
		parent, err := current.Parent(0)
		if err != nil {
			// Can't get parent, treat as end of history
			break
		}
		current = parent
	}

	// No checkpoint found
	return result
}

// promptResumeFromOlderCheckpoint asks the user if they want to resume from an older checkpoint.
func promptResumeFromOlderCheckpoint() (bool, error) {
	var confirmed bool

	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Resume from this older checkpoint?").
				Value(&confirmed),
		),
	)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get confirmation: %w", err)
	}

	return confirmed, nil
}

// checkRemoteMetadata checks if checkpoint metadata exists on origin/entire/checkpoints/v1
// and automatically fetches it if available.
func checkRemoteMetadata(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID) error {
	// Try to get remote metadata branch tree
	remoteTree, err := strategy.GetRemoteMetadataBranchTree(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Checkpoint '%s' found in commit but session metadata not available\n", checkpointID)
		fmt.Fprintf(os.Stderr, "The entire/checkpoints/v1 branch may not exist locally or on the remote.\n")
		return nil //nolint:nilerr // Informational message, not a fatal error
	}

	// Check if the checkpoint exists on the remote
	metadata, err := strategy.ReadCheckpointMetadata(remoteTree, checkpointID.Path())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Checkpoint '%s' found in commit but session metadata not available\n", checkpointID)
		return nil //nolint:nilerr // Informational message, not a fatal error
	}

	// Metadata exists on remote but not locally - fetch it automatically
	fmt.Fprintf(os.Stderr, "Fetching session metadata from origin...\n")
	if err := FetchMetadataBranch(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to fetch metadata: %v\n", err)
		fmt.Fprintf(os.Stderr, "You can try manually: git fetch origin entire/checkpoints/v1:entire/checkpoints/v1\n")
		return NewSilentError(errors.New("failed to fetch metadata"))
	}

	// Now resume the session with the fetched metadata
	return resumeSession(ctx, metadata.SessionID, checkpointID, false)
}

// resumeSession restores and displays the resume command for a specific session.
// For multi-session checkpoints, restores ALL sessions and shows commands for each.
// If force is false, prompts for confirmation when local logs have newer timestamps.
func resumeSession(ctx context.Context, sessionID string, checkpointID id.CheckpointID, force bool) error {
	// Read checkpoint metadata first to get agent type (matching rewind pattern)
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	metadataTree, err := strategy.GetMetadataBranchTree(repo)
	if err != nil {
		return fmt.Errorf("failed to get metadata branch: %w", err)
	}

	metadata, err := strategy.ReadCheckpointMetadata(metadataTree, checkpointID.Path())
	if err != nil {
		return fmt.Errorf("failed to read checkpoint metadata: %w", err)
	}

	// Resolve agent from checkpoint metadata (same as rewind)
	ag, err := strategy.ResolveAgentForRewind(metadata.Agent)
	if err != nil {
		return fmt.Errorf("failed to resolve agent: %w", err)
	}

	// Initialize logging context with agent
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "resume"), ag.Name())

	logging.Debug(logCtx, "resume session started",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("session_id", sessionID),
	)

	// Get worktree root for session directory lookup
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to get worktree root: %w", err)
	}

	sessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to determine session directory: %w", err)
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Get strategy and restore sessions using full checkpoint data
	strat := GetStrategy(ctx)

	// Use RestoreLogsOnly via LogsOnlyRestorer interface for multi-session support
	// Create a logs-only rewind point with Agent populated (same as rewind)
	point := strategy.RewindPoint{
		IsLogsOnly:   true,
		CheckpointID: checkpointID,
		Agent:        metadata.Agent,
	}

	sessions, restoreErr := strat.RestoreLogsOnly(ctx, point, force)
	if restoreErr != nil || len(sessions) == 0 {
		// Fall back to single-session restore (e.g., old checkpoints without agent metadata)
		return resumeSingleSession(ctx, ag, sessionID, checkpointID, repoRoot, force)
	}

	logging.Debug(logCtx, "resume session completed",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Int("session_count", len(sessions)),
	)

	return displayRestoredSessions(sessions)
}

// displayRestoredSessions sorts sessions by CreatedAt and prints resume commands.
// Used by both resumeSession (single checkpoint) and resumeMultipleCheckpoints (squash merge).
func displayRestoredSessions(sessions []strategy.RestoredSession) error {
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
	})

	if len(sessions) > 1 {
		fmt.Fprintf(os.Stderr, "\nRestored %d sessions. To continue, run:\n", len(sessions))
	} else if len(sessions) == 1 {
		fmt.Fprintf(os.Stderr, "Session: %s\n", sessions[0].SessionID)
		fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
	}

	for i, sess := range sessions {
		sessionAgent, err := strategy.ResolveAgentForRewind(sess.Agent)
		if err != nil {
			return fmt.Errorf("failed to resolve agent for session %s: %w", sess.SessionID, err)
		}
		cmd := sessionAgent.FormatResumeCommand(sess.SessionID)

		isLast := i == len(sessions)-1
		switch {
		case len(sessions) > 1 && isLast && sess.Prompt != "":
			fmt.Fprintf(os.Stderr, "  %s  # %s (most recent)\n", cmd, sess.Prompt)
		case len(sessions) > 1 && isLast:
			fmt.Fprintf(os.Stderr, "  %s  # (most recent)\n", cmd)
		case sess.Prompt != "":
			fmt.Fprintf(os.Stderr, "  %s  # %s\n", cmd, sess.Prompt)
		default:
			fmt.Fprintf(os.Stderr, "  %s\n", cmd)
		}
	}

	return nil
}

// resumeSingleSession restores a single session (fallback when multi-session restore fails).
// Always overwrites existing session logs to ensure consistency with checkpoint state.
// If force is false, prompts for confirmation when local log has newer timestamps.
func resumeSingleSession(ctx context.Context, ag agent.Agent, sessionID string, checkpointID id.CheckpointID, repoRoot string, force bool) error {
	sessionLogPath, err := resolveTranscriptPath(ctx, sessionID, ag)
	if err != nil {
		return fmt.Errorf("failed to resolve transcript path: %w", err)
	}

	if checkpointID.IsEmpty() {
		logging.Debug(ctx, "resume session: empty checkpoint ID",
			slog.String("checkpoint_id", checkpointID.String()),
		)
		fmt.Fprintf(os.Stderr, "Session '%s' found in commit trailer but session log not available\n", sessionID)
		fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
		fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(sessionID))
		return nil
	}

	logContent, _, err := checkpoint.LookupSessionLog(ctx, checkpointID)
	if err != nil {
		if errors.Is(err, checkpoint.ErrCheckpointNotFound) || errors.Is(err, checkpoint.ErrNoTranscript) {
			logging.Debug(ctx, "resume session completed (no metadata)",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("session_id", sessionID),
			)
			fmt.Fprintf(os.Stderr, "Session '%s' found in commit trailer but session log not available\n", sessionID)
			fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
			fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(sessionID))
			return nil
		}
		logging.Error(ctx, "resume session failed",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to get session log: %w", err)
	}

	// Check if local file has newer timestamps than checkpoint
	if !force {
		localTime := paths.GetLastTimestampFromFile(sessionLogPath)
		checkpointTime := paths.GetLastTimestampFromBytes(logContent)
		status := strategy.ClassifyTimestamps(localTime, checkpointTime)

		if status == strategy.StatusLocalNewer {
			sessions := []strategy.SessionRestoreInfo{{
				SessionID:      sessionID,
				Status:         status,
				LocalTime:      localTime,
				CheckpointTime: checkpointTime,
			}}
			shouldOverwrite, promptErr := strategy.PromptOverwriteNewerLogs(sessions)
			if promptErr != nil {
				return fmt.Errorf("failed to get confirmation: %w", promptErr)
			}
			if !shouldOverwrite {
				fmt.Fprintf(os.Stderr, "Resume cancelled. Local session log preserved.\n")
				return nil
			}
		}
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(sessionLogPath), 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	agentSession := &agent.AgentSession{
		SessionID:  sessionID,
		AgentName:  ag.Name(),
		RepoPath:   repoRoot,
		SessionRef: sessionLogPath,
		NativeData: logContent,
	}

	// Write the session using the agent's WriteSession method
	if err := ag.WriteSession(ctx, agentSession); err != nil {
		logging.Error(ctx, "resume session failed during write",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to write session: %w", err)
	}

	logging.Debug(ctx, "resume session completed",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("session_id", sessionID),
	)

	fmt.Fprintf(os.Stderr, "Session restored to: %s\n", sessionLogPath)
	fmt.Fprintf(os.Stderr, "Session: %s\n", sessionID)
	fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
	fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(sessionID))

	return nil
}

func promptFetchFromRemote(branchName string) (bool, error) {
	var confirmed bool

	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Branch '%s' not found locally. Fetch from origin?", branchName)).
				Value(&confirmed),
		),
	)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get confirmation: %w", err)
	}

	return confirmed, nil
}

// firstLine returns the first line of a string
func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
