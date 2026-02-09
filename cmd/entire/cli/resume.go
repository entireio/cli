package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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

If newer commits exist on the branch without checkpoints (e.g., after merging main),
you'll be prompted to confirm resuming from the older checkpoint.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}
			return runResume(args[0], force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Resume from older checkpoint without confirmation")

	return cmd
}

func runResume(branchName string, force bool) error {
	// Check if we're already on this branch
	currentBranch, err := GetCurrentBranch()
	if err == nil && currentBranch == branchName {
		// Already on the branch, skip checkout
		return resumeFromCurrentBranch(branchName, force)
	}

	// Check if branch exists locally
	exists, err := BranchExistsLocally(branchName)
	if err != nil {
		return fmt.Errorf("failed to check branch: %w", err)
	}

	if !exists {
		// Branch doesn't exist locally, check if it exists on remote
		remoteExists, err := BranchExistsOnRemote(branchName)
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
		if err := FetchAndCheckoutRemoteBranch(branchName); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to checkout branch: %v\n", err)
			return NewSilentError(errors.New("failed to checkout branch"))
		}
		fmt.Fprintf(os.Stderr, "Switched to branch '%s'\n", branchName)
	} else {
		// Branch exists locally, check for uncommitted changes before checkout
		hasChanges, err := HasUncommittedChanges()
		if err != nil {
			return fmt.Errorf("failed to check for uncommitted changes: %w", err)
		}
		if hasChanges {
			return errors.New("you have uncommitted changes. Please commit or stash them first")
		}

		// Checkout the branch
		if err := CheckoutBranch(branchName); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to checkout branch: %v\n", err)
			return NewSilentError(errors.New("failed to checkout branch"))
		}
		fmt.Fprintf(os.Stderr, "Switched to branch '%s'\n", branchName)
	}

	return resumeFromCurrentBranch(branchName, force)
}

func resumeFromCurrentBranch(branchName string, force bool) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Find a commit with an Entire-Checkpoint trailer, looking at branch-only commits
	result, err := findBranchCheckpoint(repo, branchName)
	if err != nil {
		return err
	}
	if result.checkpointID.IsEmpty() {
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

	checkpointID := result.checkpointID

	// Get metadata branch tree for lookups
	metadataTree, err := strategy.GetMetadataBranchTree(repo)
	if err != nil {
		// No local metadata branch, check if remote has it
		return checkRemoteMetadata(repo, checkpointID)
	}

	// Look up metadata from sharded path
	metadata, err := strategy.ReadCheckpointMetadata(metadataTree, checkpointID.Path())
	if err != nil {
		// Checkpoint exists in commit but no local metadata - check remote
		return checkRemoteMetadata(repo, checkpointID)
	}

	return resumeSession(metadata.SessionID, checkpointID, force)
}

// branchCheckpointResult contains the result of searching for a checkpoint on a branch.
type branchCheckpointResult struct {
	checkpointID      id.CheckpointID
	commitHash        string
	commitMessage     string
	newerCommitsExist bool // true if there are branch-only commits (not merge commits) without checkpoints
	newerCommitCount  int  // count of branch-only commits without checkpoints
}

// findBranchCheckpoint finds the most recent commit with an Entire-Checkpoint trailer
// among commits that are unique to this branch (not reachable from the default branch).
// This handles the case where main has been merged into the feature branch.
func findBranchCheckpoint(repo *git.Repository, branchName string) (*branchCheckpointResult, error) {
	result := &branchCheckpointResult{}

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
	if cpID, found := trailers.ParseCheckpoint(headCommit.Message); found {
		result.checkpointID = cpID
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
func findCheckpointInHistory(start *object.Commit, stopAt *plumbing.Hash) *branchCheckpointResult {
	result := &branchCheckpointResult{}
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
		if cpID, found := trailers.ParseCheckpoint(current.Message); found {
			result.checkpointID = cpID
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
func checkRemoteMetadata(repo *git.Repository, checkpointID id.CheckpointID) error {
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
	if err := FetchMetadataBranch(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to fetch metadata: %v\n", err)
		fmt.Fprintf(os.Stderr, "You can try manually: git fetch origin entire/checkpoints/v1:entire/checkpoints/v1\n")
		return NewSilentError(errors.New("failed to fetch metadata"))
	}

	// Now resume the session with the fetched metadata
	return resumeSession(metadata.SessionID, checkpointID, false)
}

// resumeSession restores and displays the resume command for a specific session.
// For multi-session checkpoints, restores ALL sessions and shows commands for each.
// If force is false, prompts for confirmation when local logs have newer timestamps.
func resumeSession(sessionID string, checkpointID id.CheckpointID, force bool) error {
	// Get the current agent (auto-detect or use default)
	ag, err := agent.Detect()
	if err != nil {
		ag = agent.Default()
		if ag == nil {
			return fmt.Errorf("no agent available: %w", err)
		}
	}

	// Initialize logging context with agent
	ctx := logging.WithAgent(logging.WithComponent(context.Background(), "resume"), ag.Name())

	logging.Debug(ctx, "resume session started",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("session_id", sessionID),
	)

	// Get repo root for session directory lookup
	// Use repo root instead of CWD because Claude stores sessions per-repo,
	// and running from a subdirectory would look up the wrong session directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
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
	strat := GetStrategy()

	// Use RestoreLogsOnly via LogsOnlyRestorer interface for multi-session support
	if restorer, ok := strat.(strategy.LogsOnlyRestorer); ok {
		// Create a logs-only rewind point to trigger full multi-session restore
		point := strategy.RewindPoint{
			IsLogsOnly:   true,
			CheckpointID: checkpointID,
		}

		if err := restorer.RestoreLogsOnly(point, force); err != nil {
			// Fall back to single-session restore
			return resumeSingleSession(ctx, ag, sessionID, checkpointID, sessionDir, repoRoot, force)
		}

		// Get checkpoint metadata to show all sessions
		repo, err := openRepository()
		if err != nil {
			// Just show the primary session - graceful fallback
			agentSID := ag.ExtractAgentSessionID(sessionID)
			fmt.Fprintf(os.Stderr, "Session: %s\n", sessionID)
			fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
			fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSID))
			return nil //nolint:nilerr // Graceful fallback to single session
		}

		metadataTree, err := strategy.GetMetadataBranchTree(repo)
		if err != nil {
			agentSID := ag.ExtractAgentSessionID(sessionID)
			fmt.Fprintf(os.Stderr, "Session: %s\n", sessionID)
			fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
			fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSID))
			return nil //nolint:nilerr // Graceful fallback to single session
		}

		metadata, err := strategy.ReadCheckpointMetadata(metadataTree, checkpointID.Path())
		if err != nil || metadata.SessionCount <= 1 {
			// Single session or can't read metadata - show standard single session output
			agentSID := ag.ExtractAgentSessionID(sessionID)
			fmt.Fprintf(os.Stderr, "Session: %s\n", sessionID)
			fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
			fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSID))
			return nil //nolint:nilerr // Graceful fallback to single session
		}

		// Multi-session: show all resume commands with prompts
		checkpointPath := checkpointID.Path()
		sessionPrompts := strategy.ReadAllSessionPromptsFromTree(metadataTree, checkpointPath, metadata.SessionCount, metadata.SessionIDs)

		logging.Debug(ctx, "resume session completed (multi-session)",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.Int("session_count", metadata.SessionCount),
		)

		fmt.Fprintf(os.Stderr, "\nRestored %d sessions. To continue, run:\n", metadata.SessionCount)
		for i, sid := range metadata.SessionIDs {
			agentSID := ag.ExtractAgentSessionID(sid)
			cmd := ag.FormatResumeCommand(agentSID)

			var prompt string
			if i < len(sessionPrompts) {
				prompt = sessionPrompts[i]
			}

			if i == len(metadata.SessionIDs)-1 {
				if prompt != "" {
					fmt.Fprintf(os.Stderr, "  %s  # %s (most recent)\n", cmd, prompt)
				} else {
					fmt.Fprintf(os.Stderr, "  %s  # (most recent)\n", cmd)
				}
			} else {
				if prompt != "" {
					fmt.Fprintf(os.Stderr, "  %s  # %s\n", cmd, prompt)
				} else {
					fmt.Fprintf(os.Stderr, "  %s\n", cmd)
				}
			}
		}

		return nil
	}

	// Strategy doesn't support LogsOnlyRestorer, fall back to single session
	return resumeSingleSession(ctx, ag, sessionID, checkpointID, sessionDir, repoRoot, force)
}

// resumeSingleSession restores a single session (fallback when multi-session restore fails).
// Always overwrites existing session logs to ensure consistency with checkpoint state.
// If force is false, prompts for confirmation when local log has newer timestamps.
func resumeSingleSession(ctx context.Context, ag agent.Agent, sessionID string, checkpointID id.CheckpointID, sessionDir, repoRoot string, force bool) error {
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionLogPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	if checkpointID.IsEmpty() {
		logging.Debug(ctx, "resume session: empty checkpoint ID",
			slog.String("checkpoint_id", checkpointID.String()),
		)
		fmt.Fprintf(os.Stderr, "Session '%s' found in commit trailer but session log not available\n", sessionID)
		fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
		fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSessionID))
		return nil
	}

	logContent, _, err := checkpoint.LookupSessionLog(checkpointID)
	if err != nil {
		if errors.Is(err, checkpoint.ErrCheckpointNotFound) || errors.Is(err, checkpoint.ErrNoTranscript) {
			logging.Debug(ctx, "resume session completed (no metadata)",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("session_id", sessionID),
			)
			fmt.Fprintf(os.Stderr, "Session '%s' found in commit trailer but session log not available\n", sessionID)
			fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
			fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSessionID))
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

	// Create an AgentSession with the native data
	agentSession := &agent.AgentSession{
		SessionID:  agentSessionID,
		AgentName:  ag.Name(),
		RepoPath:   repoRoot,
		SessionRef: sessionLogPath,
		NativeData: logContent,
	}

	// Write the session using the agent's WriteSession method
	if err := ag.WriteSession(agentSession); err != nil {
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
	fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSessionID))

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
