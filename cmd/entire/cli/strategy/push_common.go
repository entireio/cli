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
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// pushSessionsBranchCommon is the shared implementation for pushing session branches.
// Used by both manual-commit and auto-commit strategies.
// By default, session logs are pushed automatically alongside user pushes.
// Configuration (stored in .entire/settings.json under strategy_options.push_sessions):
//   - false: disable automatic pushing
//   - true or not set: push automatically (default)
func pushSessionsBranchCommon(remote, branchName string) error {
	// Check if pushing is disabled
	if isPushSessionsDisabled() {
		return nil
	}

	repo, err := OpenRepository()
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

	return doPushSessionsBranch(remote, branchName)
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
func isPushSessionsDisabled() bool {
	s, err := settings.Load()
	if err != nil {
		return false // Default: push is enabled
	}
	return s.IsPushSessionsDisabled()
}

// doPushSessionsBranch pushes the sessions branch to the remote.
func doPushSessionsBranch(remote, branchName string) error {
	fmt.Fprintf(os.Stderr, "[entire] Pushing session logs to %s...\n", remote)

	// Try pushing first
	if err := tryPushSessionsCommon(remote, branchName); err == nil {
		return nil
	}

	// Push failed - likely non-fast-forward. Try to fetch and merge.
	fmt.Fprintf(os.Stderr, "[entire] Syncing with remote session logs...\n")

	if err := fetchAndMergeSessionsCommon(remote, branchName); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync sessions: %v\n", err)
		return nil // Don't fail the main push
	}

	// Try pushing again after merge
	if err := tryPushSessionsCommon(remote, branchName); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push sessions after sync: %v\n", err)
	}

	return nil
}

// tryPushSessionsCommon attempts to push the sessions branch.
func tryPushSessionsCommon(remote, branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
func fetchAndMergeSessionsCommon(remote, branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", remote, branchName)
	fetchCmd.Stdin = nil
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch failed: %s", output)
	}

	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get local branch
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

	// Get remote (FETCH_HEAD)
	fetchHeadRef, err := repo.Reference(plumbing.ReferenceName("FETCH_HEAD"), true)
	if err != nil {
		return fmt.Errorf("failed to get FETCH_HEAD: %w", err)
	}
	remoteCommit, err := repo.CommitObject(fetchHeadRef.Hash())
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
		[]plumbing.Hash{localRef.Hash(), fetchHeadRef.Hash()},
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
