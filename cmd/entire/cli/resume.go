package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
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

			// Discover external agents so checkpoints from external agents can be resolved.
			external.DiscoverAndRegister(cmd.Context())

			return runResume(cmd.Context(), cmd, args[0], force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Resume from older checkpoint without confirmation")

	return cmd
}

func runResume(ctx context.Context, cmd *cobra.Command, branchName string, force bool) error {
	// Only initialize logging when inside a git worktree to avoid
	// creating .entire/logs/ in arbitrary directories.
	if _, err := paths.WorktreeRoot(ctx); err == nil {
		logging.SetLogLevelGetter(GetLogLevel)
		if err := logging.Init(ctx, ""); err == nil {
			defer logging.Close()
		}
	}

	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	// Check if we're already on this branch
	currentBranch, err := GetCurrentBranch(ctx)
	if err == nil && currentBranch == branchName {
		// Already on the branch, skip checkout
		return resumeFromCurrentBranch(ctx, w, errW, branchName, force)
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

		// Ask user if they want to fetch from remote (--force skips the prompt)
		if !force {
			shouldFetch, err := promptFetchFromRemote(branchName)
			if err != nil {
				return err
			}
			if !shouldFetch {
				return nil
			}
		}

		// Fetch and checkout the remote branch
		fmt.Fprintf(w, "Fetching branch '%s' from origin...\n", branchName)
		if err := FetchAndCheckoutRemoteBranch(ctx, branchName); err != nil {
			fmt.Fprintf(errW, "Error: failed to checkout branch: %v\n", err)
			return NewSilentError(errors.New("failed to checkout branch"))
		}
		fmt.Fprintf(w, "✓ Switched to branch %s\n", branchName)
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
			fmt.Fprintf(errW, "Error: failed to checkout branch: %v\n", err)
			return NewSilentError(errors.New("failed to checkout branch"))
		}
		fmt.Fprintf(w, "✓ Switched to branch %s\n", branchName)
	}

	return resumeFromCurrentBranch(ctx, w, errW, branchName, force)
}

func resumeFromCurrentBranch(ctx context.Context, w, errW io.Writer, branchName string, force bool) error {
	logCtx := logging.WithComponent(ctx, "resume")

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
		fmt.Fprintf(w, "No Entire checkpoint found on branch '%s'\n", branchName)
		return nil
	}

	logging.Debug(logCtx, "found checkpoint(s) on branch",
		slog.String("branch", branchName),
		slog.Int("checkpoint_count", len(result.checkpointIDs)),
		slog.String("commit", result.commitHash[:7]),
		slog.Bool("newer_commits_exist", result.newerCommitsExist),
	)

	// If there are newer commits without checkpoints, ask for confirmation.
	// Merge commits (e.g., from merging main) don't count as "work" and are skipped silently.
	if result.newerCommitsExist && !force {
		fmt.Fprintf(w, "Found checkpoint in an older commit.\n")
		fmt.Fprintf(w, "There are %d newer commit(s) on this branch without checkpoints.\n", result.newerCommitCount)
		fmt.Fprintf(w, "Checkpoint from: %s %s\n\n", result.commitHash[:7], firstLine(result.commitMessage))

		shouldResume, err := promptResumeFromOlderCheckpoint()
		if err != nil {
			return err
		}
		if !shouldResume {
			fmt.Fprintf(w, "Resume cancelled.\n")
			return nil
		}
	}

	checkpointID := result.checkpointIDs[0]

	// Multiple checkpoints (squash merge): resolve latest by CreatedAt timestamp.
	// resolveLatestCheckpoint also returns the metadata tree so we can reuse it
	// for the ReadCheckpointMetadata call below without a redundant lookup.
	var metadataTree *object.Tree
	var freshRepo *git.Repository
	if len(result.checkpointIDs) > 1 {
		latest, tree, latestRepo, err := resolveLatestCheckpoint(ctx, result.checkpointIDs)
		if err != nil {
			// No metadata available — nothing to resume from
			logging.Warn(logCtx, "resolveLatestCheckpoint failed",
				slog.Int("checkpoint_count", len(result.checkpointIDs)),
				slog.String("error", err.Error()),
			)
			fmt.Fprintf(w, "Found %d checkpoints for commit %s but metadata is not available\n",
				len(result.checkpointIDs), result.commitHash[:7])
			return checkRemoteMetadata(ctx, w, errW, result.checkpointIDs[0])
		}
		skipped := len(result.checkpointIDs) - 1
		fmt.Fprintf(w, "Found %d checkpoints for commit %s, resuming from the latest (%d older checkpoints skipped)\n",
			len(result.checkpointIDs), result.commitHash[:7], skipped)
		checkpointID = latest
		metadataTree = tree
		freshRepo = latestRepo
	}

	// Get metadata branch tree for lookups (reuse from resolveLatestCheckpoint if available)
	if metadataTree == nil {
		// Try v2 first when enabled
		if settings.IsCheckpointsV2Enabled(ctx) {
			v2Tree, v2Repo, v2Err := getV2MetadataTree(ctx)
			if v2Err == nil {
				metadataTree = v2Tree
				freshRepo = v2Repo
			} else {
				logging.Debug(logCtx, "v2 metadata tree not available, trying v1",
					slog.String("checkpoint_id", checkpointID.String()),
					slog.String("error", v2Err.Error()),
				)
			}
		}
	}

	// Fall back to v1 if v2 didn't find it
	if metadataTree == nil {
		var treeErr error
		metadataTree, freshRepo, treeErr = getMetadataTree(ctx)
		if treeErr != nil {
			logging.Warn(logCtx, "getMetadataTree failed, checking remote",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("error", treeErr.Error()),
			)
			return checkRemoteMetadata(ctx, w, errW, checkpointID)
		}
	}

	logging.Debug(logCtx, "metadata tree obtained",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("checkpoint_path", checkpointID.Path()),
		slog.String("tree_hash", metadataTree.Hash.String()),
	)

	// Navigate to the checkpoint subtree first (uses tree objects only, no blobs).
	// This scopes the FetchingTree to only this checkpoint's files instead of
	// the entire metadata branch.
	cpSubtree, cpErr := metadataTree.Tree(checkpointID.Path())
	if cpErr != nil {
		logging.Debug(logCtx, "checkpoint subtree not found in metadata tree, trying remote",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("checkpoint_path", checkpointID.Path()),
			slog.String("tree_hash", metadataTree.Hash.String()),
			slog.String("error", cpErr.Error()),
		)
		return checkRemoteMetadata(ctx, w, errW, checkpointID)
	}

	// Log subtree details for diagnostics
	var subtreeEntryNames []string
	for _, e := range cpSubtree.Entries {
		subtreeEntryNames = append(subtreeEntryNames, fmt.Sprintf("%s(%s:%s)", e.Name, e.Mode, e.Hash.String()[:7]))
	}
	logging.Debug(logCtx, "checkpoint subtree found",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("subtree_hash", cpSubtree.Hash.String()),
		slog.Int("entry_count", len(cpSubtree.Entries)),
		slog.Any("entries", subtreeEntryNames),
	)

	// Wrap the checkpoint subtree with on-demand blob fetching.
	// Use the fresh repo's storer (not the original repo) because a fetch may have
	// created new packfiles that the original repo's storer doesn't know about.
	ft := checkpoint.NewFetchingTree(ctx, cpSubtree, freshRepo.Storer, FetchBlobsByHash)

	// Batch-prefetch all missing blobs in one network round-trip instead of
	// fetching one blob per File() call during metadata reads.
	if prefetched, pfErr := ft.PreFetch(); pfErr != nil {
		logging.Warn(logCtx, "PreFetch failed, falling back to per-blob fetching",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("error", pfErr.Error()),
		)
	} else if prefetched > 0 {
		logging.Debug(logCtx, "PreFetch completed",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.Int("blobs_fetched", prefetched),
		)
	}

	// Read metadata from checkpoint subtree (paths are relative to checkpoint root)
	metadata, err := strategy.ReadCheckpointMetadataFromSubtree(ft, checkpointID.Path())
	if err != nil {
		logging.Warn(logCtx, "ReadCheckpointMetadataFromSubtree failed, checking remote",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("subtree_hash", cpSubtree.Hash.String()),
			slog.String("error", err.Error()),
		)
		return checkRemoteMetadata(ctx, w, errW, checkpointID)
	}

	logging.Debug(logCtx, "checkpoint metadata read successfully",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("session_id", metadata.SessionID),
		slog.Int("session_count", metadata.SessionCount),
	)

	return resumeSession(ctx, w, errW, metadata, force)
}

// resolveLatestCheckpoint reads metadata for each checkpoint ID and returns
// the one with the latest CreatedAt, along with the metadata tree and fresh
// repo for reuse. It tries the local metadata branch first, then fetches from
// remote, then falls back to the remote tree directly.
func resolveLatestCheckpoint(ctx context.Context, checkpointIDs []id.CheckpointID) (id.CheckpointID, *object.Tree, *git.Repository, error) {
	var metadataTree *object.Tree
	var freshRepo *git.Repository

	// Try v2 first when enabled
	if settings.IsCheckpointsV2Enabled(ctx) {
		v2Tree, v2Repo, v2Err := getV2MetadataTree(ctx)
		if v2Err == nil {
			metadataTree = v2Tree
			freshRepo = v2Repo
		}
	}

	// Fall back to v1
	if metadataTree == nil {
		var err error
		metadataTree, freshRepo, err = getMetadataTree(ctx)
		if err != nil {
			return id.EmptyCheckpointID, nil, nil, err
		}
	}

	infoMap := make(map[id.CheckpointID]strategy.CheckpointInfo, len(checkpointIDs))
	for _, cpID := range checkpointIDs {
		// Navigate to each checkpoint's subtree, wrap with blob fetching
		cpSubtree, cpErr := metadataTree.Tree(cpID.Path())
		if cpErr != nil {
			logging.Debug(ctx, "resolveLatestCheckpoint: checkpoint subtree not found",
				slog.String("checkpoint_id", cpID.String()),
				slog.String("error", cpErr.Error()),
			)
			continue
		}
		ft := checkpoint.NewFetchingTree(ctx, cpSubtree, freshRepo.Storer, FetchBlobsByHash)
		// Batch-prefetch blobs for this checkpoint subtree.
		if _, pfErr := ft.PreFetch(); pfErr != nil {
			logging.Debug(ctx, "resolveLatestCheckpoint: PreFetch failed",
				slog.String("checkpoint_id", cpID.String()),
				slog.String("error", pfErr.Error()),
			)
		}
		metadata, metaErr := strategy.ReadCheckpointMetadataFromSubtree(ft, cpID.Path())
		if metaErr != nil {
			logging.Debug(ctx, "resolveLatestCheckpoint: checkpoint metadata read failed",
				slog.String("checkpoint_id", cpID.String()),
				slog.String("error", metaErr.Error()),
			)
			continue
		}
		infoMap[cpID] = *metadata
	}
	latest, found := strategy.ResolveLatestCheckpointFromMap(checkpointIDs, infoMap)
	if !found {
		return id.EmptyCheckpointID, nil, nil, errors.New("no checkpoint metadata found")
	}
	return latest.CheckpointID, metadataTree, freshRepo, nil
}

// getMetadataTree returns the metadata branch tree and a fresh repo handle.
// After a fetch, go-git's storer cache may be stale (new packfiles on disk
// are invisible to the repo opened before the fetch). To avoid this, each
// attempt opens a fresh repo after the fetch succeeds.
//
// Fallback order: treeless fetch → local → checkpoint_remote → full origin fetch → remote tree.
func getMetadataTree(ctx context.Context) (*object.Tree, *git.Repository, error) {
	logCtx := logging.WithComponent(ctx, "resume.getMetadataTree")

	// Helper to log ref hash for a repo's metadata branch
	logRefHash := func(repo *git.Repository, source string) {
		ref, refErr := repo.Reference(plumbing.NewBranchReferenceName("entire/checkpoints/v1"), true)
		if refErr != nil {
			logging.Debug(logCtx, "metadata branch ref not found",
				slog.String("source", source),
				slog.String("error", refErr.Error()),
			)
			return
		}
		logging.Debug(logCtx, "metadata branch ref resolved",
			slog.String("source", source),
			slog.String("ref_hash", ref.Hash().String()),
		)
	}

	// When checkpoint_remote is configured, try it first — that's where
	// checkpoint data lives. Avoids a wasted fetch from origin (which may
	// not have the metadata branch at all).
	if fetchErr := FetchMetadataFromCheckpointRemote(ctx); fetchErr == nil {
		freshRepo, freshErr := openRepository(ctx)
		if freshErr == nil {
			logRefHash(freshRepo, "checkpoint-remote")
			metadataTree, treeErr := strategy.GetMetadataBranchTree(freshRepo)
			if treeErr == nil {
				logging.Debug(logCtx, "metadata tree obtained via checkpoint remote fetch",
					slog.String("tree_hash", metadataTree.Hash.String()),
				)
				return metadataTree, freshRepo, nil
			}
			logging.Debug(logCtx, "checkpoint remote fetch succeeded but tree read failed",
				slog.String("error", treeErr.Error()),
			)
		}
	} else {
		logging.Debug(logCtx, "checkpoint remote fetch skipped or failed",
			slog.String("error", fetchErr.Error()),
		)
	}

	// Try treeless fetch from origin
	if fetchErr := FetchMetadataTreeOnly(ctx); fetchErr == nil {
		freshRepo, repoErr := openRepository(ctx)
		if repoErr == nil {
			logRefHash(freshRepo, "treeless-fetch")
			metadataTree, treeErr := strategy.GetMetadataBranchTree(freshRepo)
			if treeErr == nil {
				logging.Debug(logCtx, "metadata tree obtained via treeless fetch",
					slog.String("tree_hash", metadataTree.Hash.String()),
				)
				return metadataTree, freshRepo, nil
			}
			logging.Debug(logCtx, "treeless fetch succeeded but tree read failed",
				slog.String("error", treeErr.Error()),
			)
		}
	} else {
		logging.Debug(logCtx, "treeless fetch failed, trying local",
			slog.String("error", fetchErr.Error()),
		)
	}

	// Try local (may have been set by a prior fetch or push)
	localRepo, repoErr := openRepository(ctx)
	if repoErr == nil {
		logRefHash(localRepo, "local")
		metadataTree, err := strategy.GetMetadataBranchTree(localRepo)
		if err == nil {
			logging.Debug(logCtx, "metadata tree obtained from local branch",
				slog.String("tree_hash", metadataTree.Hash.String()),
			)
			return metadataTree, localRepo, nil
		}
		logging.Debug(logCtx, "local metadata branch not available",
			slog.String("error", err.Error()),
		)
	}

	// Fallback: full fetch from origin
	if fetchErr := FetchMetadataBranch(ctx); fetchErr == nil {
		freshRepo, repoErr := openRepository(ctx)
		if repoErr == nil {
			logRefHash(freshRepo, "full-fetch")
			metadataTree, treeErr := strategy.GetMetadataBranchTree(freshRepo)
			if treeErr == nil {
				logging.Debug(logCtx, "metadata tree obtained via full fetch",
					slog.String("tree_hash", metadataTree.Hash.String()),
				)
				return metadataTree, freshRepo, nil
			}
			logging.Debug(logCtx, "full fetch succeeded but tree read failed",
				slog.String("error", treeErr.Error()),
			)
		}
	} else {
		logging.Debug(logCtx, "full fetch failed",
			slog.String("error", fetchErr.Error()),
		)
	}

	// Try remote tree directly (origin/entire/checkpoints/v1)
	remoteRepo, repoErr := openRepository(ctx)
	if repoErr != nil {
		return nil, nil, fmt.Errorf("failed to open repository: %w", repoErr)
	}
	logRefHash(remoteRepo, "remote-tracking")
	remoteTree, remoteErr := strategy.GetRemoteMetadataBranchTree(remoteRepo)
	if remoteErr == nil {
		logging.Debug(logCtx, "metadata tree obtained from remote-tracking branch")
		return remoteTree, remoteRepo, nil
	}
	logging.Debug(logCtx, "remote metadata tree also not available",
		slog.String("error", remoteErr.Error()),
	)

	return nil, nil, fmt.Errorf("metadata branch not available: %w", remoteErr)
}

// getV2MetadataTree resolves the v2 /main ref tree with the same
// fetch fallback pattern as getMetadataTree, including checkpoint remote support.
func getV2MetadataTree(ctx context.Context) (*object.Tree, *git.Repository, error) {
	tree, repo, err := checkpoint.GetV2MetadataTree(ctx, FetchV2MainTreeOnly, FetchV2MainRef, openRepository)
	if err == nil {
		return tree, repo, nil
	}

	// Try checkpoint remote if configured (fetch ref, then read locally)
	if fetchErr := FetchV2MetadataFromCheckpointRemote(ctx); fetchErr == nil {
		tree, repo, localErr := checkpoint.GetV2MetadataTree(ctx, nil, nil, openRepository)
		if localErr == nil {
			return tree, repo, nil
		}
		logging.Debug(ctx, "v2 checkpoint remote fetch succeeded but tree read failed",
			slog.String("error", localErr.Error()),
		)
	} else {
		logging.Debug(ctx, "v2 checkpoint remote fetch skipped or failed",
			slog.String("error", fetchErr.Error()),
		)
	}

	return nil, nil, fmt.Errorf("failed to get v2 metadata tree: %w", err)
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

// checkRemoteMetadata checks if checkpoint metadata exists on the remote and
// automatically fetches it if available. Tries v2 refs first when enabled.
// When a checkpoint_remote is configured, fetches from there. Otherwise falls back to origin.
func checkRemoteMetadata(ctx context.Context, w, errW io.Writer, checkpointID id.CheckpointID) error {
	logCtx := logging.WithComponent(ctx, "resume.checkRemoteMetadata")

	// Try v2 /main ref first when enabled.
	// Only fetches /main (metadata), not /full/* (transcripts). If /full/* refs
	// aren't local, RestoreLogsOnly falls back to v1 for transcript data.
	if settings.IsCheckpointsV2Enabled(ctx) {
		v2Tree, v2Repo, v2Err := getV2MetadataTree(ctx)
		if v2Err == nil {
			cpSubtree, cpErr := v2Tree.Tree(checkpointID.Path())
			if cpErr == nil {
				ft := checkpoint.NewFetchingTree(ctx, cpSubtree, v2Repo.Storer, FetchBlobsByHash)
				if _, pfErr := ft.PreFetch(); pfErr != nil {
					logging.Debug(logCtx, "checkRemoteMetadata v2: PreFetch failed",
						slog.String("error", pfErr.Error()),
					)
				}
				metadata, metaErr := strategy.ReadCheckpointMetadataFromSubtree(ft, checkpointID.Path())
				if metaErr == nil {
					return resumeSession(ctx, w, errW, metadata, false)
				}
			}
		}
		logging.Debug(logCtx, "v2 remote metadata not available, trying v1",
			slog.String("checkpoint_id", checkpointID.String()),
		)
	}

	// Open a fresh repo to avoid stale packfile index issues
	repo, repoErr := openRepository(ctx)
	if repoErr != nil {
		logging.Warn(logCtx, "failed to open repository for remote check",
			slog.String("error", repoErr.Error()),
		)
		fmt.Fprintf(errW, "Checkpoint '%s' found in commit but session metadata not available\n", checkpointID)
		return nil
	}

	// Resolve checkpoint remote URL once; reuse for both fetch and error message.
	hasCheckpointRemote := remote.Configured(ctx)

	// Try checkpoint_remote first if configured and resolved (that's where checkpoints are stored)
	var checkpointURL string
	var resolveErr error
	if hasCheckpointRemote {
		checkpointURL, resolveErr = remote.FetchURL(ctx)
		if resolveErr == nil {
			if fetchErr := strategy.FetchMetadataBranch(ctx, checkpointURL); fetchErr == nil {
				freshRepo, freshErr := openRepository(ctx)
				if freshErr != nil {
					logging.Debug(logCtx, "checkpoint remote: open repository failed after fetch",
						slog.String("error", freshErr.Error()),
					)
				} else if metadataTree, treeErr := strategy.GetMetadataBranchTree(freshRepo); treeErr != nil {
					logging.Debug(logCtx, "checkpoint remote: fetch succeeded but tree read failed",
						slog.String("error", treeErr.Error()),
					)
				} else if metadata, err := tryReadCheckpointFromTree(ctx, metadataTree, freshRepo, checkpointID); err != nil {
					logging.Debug(logCtx, "checkpoint remote: tree read succeeded but checkpoint metadata read failed",
						slog.String("checkpoint_id", checkpointID.String()),
						slog.String("error", err.Error()),
					)
				} else {
					return resumeSession(ctx, w, errW, metadata, false)
				}
			} else {
				logging.Debug(logCtx, "checkpoint remote fetch failed",
					slog.String("error", fetchErr.Error()),
				)
			}
		}
	}

	// Fall back to origin's remote-tracking branch
	if remoteTree, treeErr := strategy.GetRemoteMetadataBranchTree(repo); treeErr == nil {
		if metadata, err := tryReadCheckpointFromTree(ctx, remoteTree, repo, checkpointID); err == nil {
			return resumeSession(ctx, w, errW, metadata, false)
		}
	}

	// Nothing worked — print helpful error message
	if hasCheckpointRemote {
		if resolveErr != nil {
			fmt.Fprintf(errW, "Checkpoint '%s' found in commit but the checkpoint remote URL could not be resolved: %s\n", checkpointID, resolveErr)
		} else {
			fmt.Fprintf(errW, "Checkpoint '%s' found in commit but its metadata could not be fetched from the checkpoint remote.\n", checkpointID)
		}
		fmt.Fprintf(errW, "Ensure you have access to the checkpoint remote configured in .entire/settings.json.\n")
	} else {
		fmt.Fprintf(errW, "Checkpoint '%s' found in commit but the entire/checkpoints/v1 branch is not available locally or on the remote.\n", checkpointID)
		fmt.Fprintf(errW, "This can happen if the metadata branch was not pushed. Try:\n")
		fmt.Fprintf(errW, "  git fetch origin entire/checkpoints/v1:entire/checkpoints/v1\n")
	}
	return nil
}

// tryReadCheckpointFromTree attempts to read checkpoint metadata from a metadata tree.
func tryReadCheckpointFromTree(ctx context.Context, tree *object.Tree, repo *git.Repository, checkpointID id.CheckpointID) (*strategy.CheckpointInfo, error) {
	cpSubtree, cpErr := tree.Tree(checkpointID.Path())
	if cpErr != nil {
		return nil, fmt.Errorf("checkpoint subtree not found: %w", cpErr)
	}
	ft := checkpoint.NewFetchingTree(ctx, cpSubtree, repo.Storer, FetchBlobsByHash)
	if _, pfErr := ft.PreFetch(); pfErr != nil {
		logging.Debug(ctx, "tryReadCheckpointFromTree: PreFetch failed",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("error", pfErr.Error()),
		)
	}
	metadata, err := strategy.ReadCheckpointMetadataFromSubtree(ft, checkpointID.Path())
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint metadata: %w", err)
	}
	return metadata, nil
}

// resumeSession restores and displays the resume command for a specific session.
// For multi-session checkpoints, restores ALL sessions and shows commands for each.
// If force is false, prompts for confirmation when local logs have newer timestamps.
// The caller must provide the already-resolved checkpoint metadata to avoid redundant lookups
// and to support both local and remote metadata trees.
func resumeSession(ctx context.Context, w, errW io.Writer, metadata *strategy.CheckpointInfo, force bool) error {
	checkpointID := metadata.CheckpointID
	sessionID := metadata.SessionID

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

	sessions, restoreErr := strat.RestoreLogsOnly(ctx, w, errW, point, force)
	if restoreErr != nil || len(sessions) == 0 {
		// Fall back to single-session restore (e.g., old checkpoints without agent metadata)
		return resumeSingleSession(ctx, w, errW, ag, sessionID, checkpointID, repoRoot, force)
	}

	logging.Debug(logCtx, "resume session completed",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Int("session_count", len(sessions)),
	)

	return displayRestoredSessions(w, sessions)
}

// displayRestoredSessions sorts sessions by CreatedAt and prints resume commands.
func displayRestoredSessions(w io.Writer, sessions []strategy.RestoredSession) error {
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
	})

	if len(sessions) > 1 {
		fmt.Fprintf(w, "\n✓ Restored %d sessions. To continue:\n", len(sessions))
	} else if len(sessions) == 1 {
		fmt.Fprintf(w, "✓ Restored session %s.\n", sessions[0].SessionID)
		fmt.Fprintf(w, "\nTo continue this session:\n")
	}

	isMulti := len(sessions) > 1
	for i, sess := range sessions {
		sessionAgent, err := strategy.ResolveAgentForRewind(sess.Agent)
		if err != nil {
			return fmt.Errorf("failed to resolve agent for session %s: %w", sess.SessionID, err)
		}
		printSessionCommand(w, sessionAgent.FormatResumeCommand(sess.SessionID), sess.Prompt, isMulti, i == len(sessions)-1)
	}

	return nil
}

// resumeSingleSession restores a single session (fallback when multi-session restore fails).
// Always overwrites existing session logs to ensure consistency with checkpoint state.
// If force is false, prompts for confirmation when local log has newer timestamps.
func resumeSingleSession(ctx context.Context, w, errW io.Writer, ag agent.Agent, sessionID string, checkpointID id.CheckpointID, repoRoot string, force bool) error {
	sessionLogPath, err := resolveTranscriptPath(ctx, sessionID, ag)
	if err != nil {
		return fmt.Errorf("failed to resolve transcript path: %w", err)
	}

	if checkpointID.IsEmpty() {
		logging.Debug(ctx, "resume session: empty checkpoint ID",
			slog.String("checkpoint_id", checkpointID.String()),
		)
		fmt.Fprintf(w, "Session '%s' found in commit trailer but session log not available\n", sessionID)
		fmt.Fprintf(w, "\nTo continue this session:\n")
		fmt.Fprintf(w, "  %s\n", ag.FormatResumeCommand(sessionID))
		return nil
	}

	var logContent []byte
	err = nil // Reset before v2/v1 resolution to avoid stale error from earlier code paths
	if settings.IsCheckpointsV2Enabled(ctx) {
		repo, repoErr := openRepository(ctx)
		if repoErr == nil {
			v2URL, fetchRemoteErr := remote.FetchURL(ctx)
			if fetchRemoteErr != nil {
				logging.Debug(ctx, "resume: using origin for v2 session log fetch remote",
					slog.String("error", fetchRemoteErr.Error()),
				)
				v2URL = ""
			}
			v2Store := checkpoint.NewV2GitStore(repo, v2URL)
			var v2Err error
			logContent, _, v2Err = v2Store.GetSessionLog(ctx, checkpointID)
			if v2Err != nil {
				logging.Debug(ctx, "v2 GetSessionLog failed, falling back to v1",
					slog.String("checkpoint_id", checkpointID.String()),
					slog.String("error", v2Err.Error()),
				)
			}
		}
	}
	if len(logContent) == 0 {
		logContent, _, err = checkpoint.LookupSessionLog(ctx, checkpointID)
	}
	if err != nil {
		if errors.Is(err, checkpoint.ErrCheckpointNotFound) || errors.Is(err, checkpoint.ErrNoTranscript) {
			logging.Debug(ctx, "resume session completed (no metadata)",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("session_id", sessionID),
			)
			fmt.Fprintf(w, "Session '%s' found in commit trailer but session log not available\n", sessionID)
			fmt.Fprintf(w, "\nTo continue this session:\n")
			fmt.Fprintf(w, "  %s\n", ag.FormatResumeCommand(sessionID))
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
			shouldOverwrite, promptErr := strategy.PromptOverwriteNewerLogs(errW, sessions)
			if promptErr != nil {
				return fmt.Errorf("failed to get confirmation: %w", promptErr)
			}
			if !shouldOverwrite {
				fmt.Fprintf(w, "Resume cancelled. Local session log preserved.\n")
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

	fmt.Fprintf(w, "✓ Session restored to: %s\n", sessionLogPath)
	fmt.Fprintf(w, "  Session: %s\n", sessionID)
	fmt.Fprintf(w, "\nTo continue this session:\n")
	fmt.Fprintf(w, "  %s\n", ag.FormatResumeCommand(sessionID))

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
