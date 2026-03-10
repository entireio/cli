package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// pushSessionsBranchCommon is the shared implementation for pushing session branches.
// By default, session logs are pushed automatically alongside user pushes.
// Configuration (stored in .entire/settings.json under strategy_options.push_sessions):
//   - false: disable automatic pushing
//   - true or not set: push automatically (default)
func pushSessionsBranchCommon(ctx context.Context, remote, branchName string) error {
	// Check if pushing is disabled
	if isPushSessionsDisabled(ctx) {
		return nil
	}

	return pushBranchIfNeeded(ctx, remote, branchName)
}

// pushBranchIfNeeded pushes a branch to the remote if it has unpushed changes.
// Does not check any settings — callers are responsible for gating.
func pushBranchIfNeeded(ctx context.Context, remote, branchName string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Check if branch exists locally
	branchRef := plumbing.NewBranchReferenceName(branchName)
	localRef, err := repo.Reference(branchRef, true)
	if err != nil {
		// No branch, nothing to push
		return nil //nolint:nilerr // Expected when no sessions exist yet
	}

	// Check if there's actually something to push (local differs from remote)
	if !hasUnpushedSessionsCommon(repo, remote, localRef.Hash(), branchName) {
		// Nothing to push - skip silently
		return nil
	}

	return doPushBranch(ctx, remote, branchName)
}

// hasUnpushedSessionsCommon checks if the local branch differs from the remote.
// Returns true if there's any difference that needs syncing (local ahead, remote ahead, or diverged).
func hasUnpushedSessionsCommon(repo *git.Repository, remote string, localHash plumbing.Hash, branchName string) bool {
	// Check for remote tracking ref: refs/remotes/<remote>/<branch>
	remoteRefName := plumbing.NewRemoteReferenceName(remote, branchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		// Remote branch doesn't exist yet - we have content to push
		return true
	}

	// If local and remote point to same commit, nothing to sync
	// This is the only case where we skip - any difference needs handling
	return localHash != remoteRef.Hash()
}

// isPushSessionsDisabled checks if push_sessions is disabled in settings.
// Returns true if push_sessions is explicitly set to false.
func isPushSessionsDisabled(ctx context.Context) bool {
	s, err := settings.Load(ctx)
	if err != nil {
		return false // Default: push is enabled
	}
	return s.IsPushSessionsDisabled()
}

// doPushBranch pushes the given branch to the remote with fetch+merge recovery.
func doPushBranch(ctx context.Context, remote, branchName string) error {
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...\n", branchName, remote)

	// Try pushing first
	if err := tryPushSessionsCommon(ctx, remote, branchName); err == nil {
		return nil
	}

	// Push failed - likely non-fast-forward. Try to fetch and merge.
	fmt.Fprintf(os.Stderr, "[entire] Syncing %s with remote...\n", branchName)

	if err := fetchAndMergeSessionsCommon(ctx, remote, branchName); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync %s: %v\n", branchName, err)
		return nil // Don't fail the main push
	}

	// Try pushing again after merge
	if err := tryPushSessionsCommon(ctx, remote, branchName); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push %s after sync: %v\n", branchName, err)
	}

	return nil
}

// tryPushSessionsCommon attempts to push the sessions branch.
func tryPushSessionsCommon(ctx context.Context, remote, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Use --no-verify to prevent recursive hook calls
	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", remote, branchName)
	cmd.Stdin = nil // Disconnect stdin to prevent hanging in hook context

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if it's a non-fast-forward error (we can try to recover)
		if strings.Contains(string(output), "non-fast-forward") ||
			strings.Contains(string(output), "rejected") {
			return errors.New("non-fast-forward")
		}
		return fmt.Errorf("push failed: %s", output)
	}
	return nil
}

// fetchAndMergeSessionsCommon fetches remote sessions and merges into local using go-git.
// Since session logs are append-only (unique cond-* directories), we just combine trees.
func fetchAndMergeSessionsCommon(ctx context.Context, remote, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branchName, remote, branchName)
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", remote, refSpec)
	fetchCmd.Stdin = nil
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch failed: %s", output)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Reconcile disconnected metadata branches before merging trees.
	// The fetch above updated the remote-tracking ref, so reconciliation
	// can compare fresh local vs remote. If disconnected (empty-orphan bug),
	// this cherry-picks local commits onto remote tip, updating the local ref.
	// If reconciliation fails, abort — proceeding to tree merge on disconnected
	// branches would silently combine unrelated histories.
	if reconcileErr := ReconcileDisconnectedMetadataBranch(ctx, repo, os.Stderr); reconcileErr != nil {
		return fmt.Errorf("metadata reconciliation failed: %w", reconcileErr)
	}

	// Get local branch (re-read after potential reconciliation update)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}
	localCommit, err := repo.CommitObject(localRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get local commit: %w", err)
	}
	localTree, err := localCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get local tree: %w", err)
	}

	// Get remote tracking ref (updated by the fetch above)
	remoteRefName := plumbing.NewRemoteReferenceName(remote, branchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get remote ref: %w", err)
	}
	remoteCommit, err := repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get remote commit: %w", err)
	}
	remoteTree, err := remoteCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get remote tree: %w", err)
	}

	// Flatten both trees and combine entries
	// Session logs have unique cond-* directories, so no conflicts expected
	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, localTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten local tree: %w", err)
	}
	if err := checkpoint.FlattenTree(repo, remoteTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten remote tree: %w", err)
	}

	// Build merged tree
	mergedTreeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		return fmt.Errorf("failed to build merged tree: %w", err)
	}

	// Create merge commit with both parents
	mergeCommitHash, err := createMergeCommitCommon(repo, mergedTreeHash,
		[]plumbing.Hash{localRef.Hash(), remoteRef.Hash()},
		"Merge remote session logs")
	if err != nil {
		return fmt.Errorf("failed to create merge commit: %w", err)
	}

	// Update branch ref
	newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), mergeCommitHash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update branch ref: %w", err)
	}

	return nil
}

// PushTrailsBranch pushes the entire/trails/v1 branch to the remote.
// Trails are always pushed regardless of the push_sessions setting.
func PushTrailsBranch(ctx context.Context, remote string) error {
	return pushBranchIfNeeded(ctx, remote, paths.TrailsBranchName)
}

// createMergeCommitCommon creates a merge commit with multiple parents.
func createMergeCommitCommon(repo *git.Repository, treeHash plumbing.Hash, parents []plumbing.Hash, message string) (plumbing.Hash, error) {
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	now := time.Now()
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:     treeHash,
		ParentHashes: parents,
		Author:       sig,
		Committer:    sig,
		Message:      message,
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit: %w", err)
	}

	return hash, nil
}
