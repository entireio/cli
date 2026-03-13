package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// pushBranchIfNeeded pushes a branch to the given target if it has unpushed changes.
// The target can be a remote name (e.g., "origin") or a URL for direct push.
// When pushing to a URL, the "has unpushed" optimization is skipped since there are
// no remote tracking refs — git itself handles the no-op case.
// Does not check any settings — callers are responsible for gating.
func pushBranchIfNeeded(ctx context.Context, target, branchName string) error {
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

	// Only check remote tracking refs when target is a remote name (not a URL).
	// URLs don't have tracking refs, so we always attempt the push and let git handle it.
	if !isURL(target) && !hasUnpushedSessionsCommon(repo, target, localRef.Hash(), branchName) {
		return nil
	}

	return doPushBranch(ctx, target, branchName)
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

// doPushBranch pushes the given branch to the target with fetch+merge recovery.
// The target can be a remote name or a URL.
func doPushBranch(ctx context.Context, target, branchName string) error {
	displayTarget := target
	if isURL(target) {
		displayTarget = "checkpoint remote"
	}

	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", branchName, displayTarget)
	stop := startProgressDots(os.Stderr)

	// Try pushing first
	if err := tryPushSessionsCommon(ctx, target, branchName); err == nil {
		stop(" done")
		return nil
	}
	stop("")

	// Push failed - likely non-fast-forward. Try to fetch and merge.
	fmt.Fprintf(os.Stderr, "[entire] Syncing %s with remote...", branchName)
	stop = startProgressDots(os.Stderr)

	if err := fetchAndMergeSessionsCommon(ctx, target, branchName); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync %s: %v\n", branchName, err)
		printCheckpointRemoteHint(target)
		return nil // Don't fail the main push
	}
	stop(" done")

	// Try pushing again after merge
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", branchName, displayTarget)
	stop = startProgressDots(os.Stderr)

	if err := tryPushSessionsCommon(ctx, target, branchName); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push %s after sync: %v\n", branchName, err)
		printCheckpointRemoteHint(target)
	} else {
		stop(" done")
	}

	return nil
}

// printCheckpointRemoteHint prints a hint when a push to a checkpoint URL fails.
// Only prints when the target is a URL (not the user's default remote).
func printCheckpointRemoteHint(target string) {
	if !isURL(target) {
		return
	}
	fmt.Fprintln(os.Stderr, "[entire] A checkpoint remote is configured in Entire settings (.entire/settings.json or .entire/settings.local.json) but could not be reached.")
	fmt.Fprintln(os.Stderr, "[entire] Checkpoints are saved locally but not synced. Ensure you have access to the checkpoint remote.")
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
// The target can be a remote name or a URL.
func fetchAndMergeSessionsCommon(ctx context.Context, target, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Determine fetch refspec. When target is a URL, use a temp ref;
	// when it's a remote name, use the standard remote-tracking ref.
	var fetchedRefName plumbing.ReferenceName
	var refSpec string
	if isURL(target) {
		tmpRef := "refs/entire-fetch-tmp/" + branchName
		refSpec = fmt.Sprintf("+refs/heads/%s:%s", branchName, tmpRef)
		fetchedRefName = plumbing.ReferenceName(tmpRef)
	} else {
		refSpec = fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branchName, target, branchName)
		fetchedRefName = plumbing.NewRemoteReferenceName(target, branchName)
	}

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", target, refSpec)
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

	// Get fetched ref (remote-tracking or temp ref, updated by the fetch above)
	remoteRef, err := repo.Reference(fetchedRefName, true)
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

	// Clean up temp ref if we used one (best-effort, not critical if it fails)
	if isURL(target) {
		_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
	}

	return nil
}

// startProgressDots prints dots to w every second until the returned stop function
// is called. The stop function prints the given suffix and a newline.
func startProgressDots(w io.Writer) func(suffix string) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprint(w, ".")
			}
		}
	}()
	return func(suffix string) {
		close(done)
		<-stopped // Wait for goroutine to finish before writing suffix
		fmt.Fprintln(w, suffix)
	}
}

// isURL returns true if the target looks like a URL rather than a git remote name.
func isURL(target string) bool {
	return strings.Contains(target, "://") || strings.Contains(target, "@")
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
