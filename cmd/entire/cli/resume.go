package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

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
			return err
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
			return err
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
	if result.checkpointID == "" {
		fmt.Fprintf(os.Stderr, "No Entire checkpoint found on branch '%s'\n", branchName)
		return nil
	}

	// If there are actual branch work commits (not just merge commits) without checkpoints,
	// ask for confirmation. Merge commits (e.g., from merging main) don't count as "work".
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
	} else if result.mergeCommitsOnly {
		// Just merge commits between HEAD and checkpoint - no warning needed, this is the
		// common "merged main into feature branch" scenario
		fmt.Fprintf(os.Stderr, "Resuming from checkpoint (skipping merge commit(s)).\n")
	}

	checkpointID := result.checkpointID

	// Get metadata branch tree for lookups
	metadataTree, err := strategy.GetMetadataBranchTree(repo)
	if err != nil {
		// No local metadata branch, check if remote has it
		return checkRemoteMetadata(repo, checkpointID)
	}

	// Look up metadata from sharded path
	metadata, err := strategy.ReadCheckpointMetadata(metadataTree, paths.CheckpointPath(checkpointID))
	if err != nil {
		// Checkpoint exists in commit but no local metadata - check remote
		return checkRemoteMetadata(repo, checkpointID)
	}

	return resumeSession(metadata.SessionID, checkpointID)
}

// branchCheckpointResult contains the result of searching for a checkpoint on a branch.
type branchCheckpointResult struct {
	checkpointID      string
	commitHash        string
	commitMessage     string
	newerCommitsExist bool // true if there are branch-only commits (not merge commits) without checkpoints
	newerCommitCount  int  // count of branch-only commits without checkpoints
	mergeCommitsOnly  bool // true if ALL newer commits are merge commits (no actual branch work)
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
	if checkpointID, found := paths.ParseCheckpointTrailer(headCommit.Message); found {
		result.checkpointID = checkpointID
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
	mergeCommits := 0      // Merge commits without checkpoints
	const maxCommits = 100 // Limit search depth
	totalChecked := 0

	current := start
	for current != nil && totalChecked < maxCommits {
		// Stop if we've reached the boundary
		if stopAt != nil && current.Hash == *stopAt {
			break
		}

		// Check for checkpoint trailer
		if checkpointID, found := paths.ParseCheckpointTrailer(current.Message); found {
			result.checkpointID = checkpointID
			result.commitHash = current.Hash.String()
			result.commitMessage = current.Message
			// Only warn about branch work commits, not merge commits
			result.newerCommitsExist = branchWorkCommits > 0
			result.newerCommitCount = branchWorkCommits
			result.mergeCommitsOnly = branchWorkCommits == 0 && mergeCommits > 0
			return result
		}

		// Track what kind of commit this is
		if current.NumParents() > 1 {
			// This is a merge commit (bringing in another branch)
			mergeCommits++
		} else {
			// This is a regular commit (actual branch work)
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

// checkRemoteMetadata checks if checkpoint metadata exists on origin/entire/sessions
// and provides guidance to the user.
func checkRemoteMetadata(repo *git.Repository, checkpointID string) error {
	// Try to get remote metadata branch tree
	remoteTree, err := strategy.GetRemoteMetadataBranchTree(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Checkpoint '%s' found in commit but session metadata not available\n", checkpointID)
		fmt.Fprintf(os.Stderr, "The entire/sessions branch may not exist locally or on the remote.\n")
		return nil //nolint:nilerr // Informational message, not a fatal error
	}

	// Check if the checkpoint exists on the remote
	_, err = strategy.ReadCheckpointMetadata(remoteTree, paths.CheckpointPath(checkpointID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Checkpoint '%s' found in commit but session metadata not available\n", checkpointID)
		return nil //nolint:nilerr // Informational message, not a fatal error
	}

	// Metadata exists on remote but not locally
	fmt.Fprintf(os.Stderr, "Checkpoint '%s' found in commit but session metadata not available locally\n", checkpointID)
	fmt.Fprintf(os.Stderr, "The metadata exists on origin. To fetch it, run:\n")
	fmt.Fprintf(os.Stderr, "  git fetch origin entire/sessions:entire/sessions\n")
	fmt.Fprintf(os.Stderr, "\nThen run this command again.\n")
	return nil
}

// resumeSession restores and displays the resume command for a specific session.
func resumeSession(sessionID, checkpointID string) error {
	// Get the current agent (auto-detect or use default)
	ag, err := agent.Detect()
	if err != nil {
		ag = agent.Default()
		if ag == nil {
			return fmt.Errorf("no agent available: %w", err)
		}
	}

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

	// Extract agent-specific session ID from Entire session ID
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionLogPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	// Check if session log already exists
	if !fileExists(sessionLogPath) {
		// Restore the session log
		strat := GetStrategy()

		logContent, _, err := strat.GetSessionLog(checkpointID)
		if err != nil {
			if errors.Is(err, strategy.ErrNoMetadata) {
				fmt.Fprintf(os.Stderr, "Session '%s' found in commit trailer but session log not available\n", sessionID)
				fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
				fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSessionID))
				return nil
			}
			return fmt.Errorf("failed to get session log: %w", err)
		}

		// Create an AgentSession with the native data
		agentSession := &agent.AgentSession{
			SessionID:  agentSessionID,
			AgentName:  ag.Name(),
			RepoPath:   repoRoot,
			SessionRef: sessionLogPath,
			NativeData: logContent,
		}

		// Create directory if it doesn't exist
		if err := os.MkdirAll(sessionDir, 0o700); err != nil {
			return fmt.Errorf("failed to create session directory: %w", err)
		}

		// Write the session using the agent's WriteSession method
		if err := ag.WriteSession(agentSession); err != nil {
			return fmt.Errorf("failed to write session: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Session restored to: %s\n", sessionLogPath)
	}

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
