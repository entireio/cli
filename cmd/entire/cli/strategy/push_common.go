package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/settings"

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
	if result, err := tryPushSessionsCommon(ctx, target, branchName); err == nil {
		finishPush(ctx, stop, result, target)
		return nil
	}
	stop("")

	// Push failed - likely non-fast-forward. Try to fetch and rebase.
	fmt.Fprintf(os.Stderr, "[entire] Syncing %s with remote...", branchName)
	stop = startProgressDots(os.Stderr)

	if err := fetchAndRebaseSessionsCommon(ctx, target, branchName); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync %s: %v\n", branchName, err)
		printCheckpointRemoteHint(target)
		return nil // Don't fail the main push
	}
	stop(" done")

	// Try pushing again after rebase
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", branchName, displayTarget)
	stop = startProgressDots(os.Stderr)

	if result, err := tryPushSessionsCommon(ctx, target, branchName); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push %s after sync: %v\n", branchName, err)
		printCheckpointRemoteHint(target)
	} else {
		finishPush(ctx, stop, result, target)
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

// settingsHintOnce ensures the settings commit hint prints at most once per process.
var settingsHintOnce sync.Once

// printSettingsCommitHint prints a hint after a successful checkpoint remote push
// when the committed .entire/settings.json does not contain a checkpoint_remote config.
// entire.io discovers the external checkpoint repo by reading the committed project
// settings, so the checkpoint_remote must be present in HEAD:.entire/settings.json
// (not just in settings.local.json or uncommitted local changes).
// Uses sync.Once to avoid duplicates when multiple branches/refs are pushed in a
// single pre-push invocation.
func printSettingsCommitHint(ctx context.Context, target string) {
	if !isURL(target) {
		return
	}
	settingsHintOnce.Do(func() {
		if isCheckpointRemoteCommitted(ctx) {
			return
		}
		fmt.Fprintln(os.Stderr, "[entire] Note: Checkpoints were pushed to a separate checkpoint remote, but .entire/settings.json does not contain checkpoint_remote in the latest commit. entire.io will not be able to discover these checkpoints until checkpoint_remote is committed and pushed in .entire/settings.json.")
	})
}

// isCheckpointRemoteCommitted returns true if the committed .entire/settings.json
// at HEAD contains a valid checkpoint_remote configuration. This is the true
// discoverability check: entire.io reads from committed project settings, not from
// local overrides or uncommitted changes.
func isCheckpointRemoteCommitted(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "git", "show", "HEAD:.entire/settings.json")
	output, err := cmd.Output()
	if err != nil {
		return false // file doesn't exist at HEAD
	}
	// Parse the committed content and check for checkpoint_remote
	committed, err := settings.LoadFromBytes(output)
	if err != nil {
		return false
	}
	return committed.GetCheckpointRemote() != nil
}

// pushResult describes what happened during a push attempt.
type pushResult struct {
	// upToDate is true when the remote already had all commits (nothing transferred).
	upToDate bool
}

// parsePushResult checks git push --porcelain output for ref status flags.
// In porcelain mode, each ref gets a tab-delimited status line:
//
//	<flag>\t<from>:<to>\t<summary>
//
// where flag '=' means the ref was already up-to-date. This is locale-independent,
// unlike the human-readable "Everything up-to-date" message.
func parsePushResult(output string) pushResult {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "=\t") {
			return pushResult{upToDate: true}
		}
	}
	return pushResult{upToDate: false}
}

// finishPush stops the progress dots and prints "already up-to-date" or "done"
// depending on the push result. Only prints the settings commit hint when new
// content was actually pushed.
func finishPush(ctx context.Context, stop func(string), result pushResult, target string) {
	if result.upToDate {
		stop(" already up-to-date")
	} else {
		stop(" done")
		printSettingsCommitHint(ctx, target)
	}
}

// tryPushSessionsCommon attempts to push the sessions branch.
func tryPushSessionsCommon(ctx context.Context, remote, branchName string) (pushResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Use --no-verify to prevent recursive hook calls.
	// Use --porcelain for machine-readable, locale-independent output.
	cmd := CheckpointGitCommand(ctx, remote, "push", "--no-verify", "--porcelain", remote, branchName)

	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	if err != nil {
		// Check if it's a non-fast-forward error (we can try to recover)
		if strings.Contains(outputStr, "non-fast-forward") ||
			strings.Contains(outputStr, "rejected") {
			return pushResult{}, errors.New("non-fast-forward")
		}
		return pushResult{}, fmt.Errorf("push failed: %s", outputStr)
	}

	return parsePushResult(outputStr), nil
}

// fetchAndRebaseSessionsCommon fetches remote sessions and rebases local commits
// on top of the remote tip. Since checkpoint shards use unique paths, rebases
// always apply cleanly.
// The target can be a remote name or a URL.
func fetchAndRebaseSessionsCommon(ctx context.Context, target, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	fetchTarget, err := ResolveFetchTarget(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve fetch target: %w", err)
	}

	// Determine fetch refspec. When the resolved fetch target is a URL, use a
	// temp ref; when it's still a remote name, use the standard remote-tracking
	// ref.
	var fetchedRefName plumbing.ReferenceName
	var refSpec string
	usedTempRef := isURL(fetchTarget)
	if usedTempRef {
		tmpRef := "refs/entire-fetch-tmp/" + branchName
		refSpec = fmt.Sprintf("+refs/heads/%s:%s", branchName, tmpRef)
		fetchedRefName = plumbing.ReferenceName(tmpRef)
	} else {
		refSpec = fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branchName, target, branchName)
		fetchedRefName = plumbing.NewRemoteReferenceName(target, branchName)
	}

	// Use git CLI for fetch (go-git's fetch can be tricky with auth).
	// Use --filter=blob:none for a partial fetch that downloads only commits
	// and trees, skipping blobs. The merge only needs the tree structure to
	// combine entries; blobs are already local or fetched on demand.
	fetchArgs := AppendFetchFilterArgs(ctx, []string{"fetch", "--no-tags", fetchTarget, refSpec})
	fetchCmd := CheckpointGitCommand(ctx, fetchTarget, fetchArgs...)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch failed: %s", output)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Reconcile disconnected metadata branches before rebasing.
	// The fetch above updated the remote-tracking ref, so reconciliation
	// can compare fresh local vs remote. If disconnected (empty-orphan bug),
	// this cherry-picks local commits onto remote tip, updating the local ref.
	// If reconciliation fails, abort — proceeding to rebase on disconnected
	// branches would silently combine unrelated histories.
	if reconcileErr := ReconcileDisconnectedMetadataBranch(ctx, repo, fetchedRefName, os.Stderr); reconcileErr != nil {
		return fmt.Errorf("metadata reconciliation failed: %w", reconcileErr)
	}

	// Get local branch (re-read after potential reconciliation update)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}

	// Get fetched ref (remote-tracking or temp ref, updated by the fetch above)
	remoteRef, err := repo.Reference(fetchedRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get remote ref: %w", err)
	}

	// If local is already at or behind remote, fast-forward
	if localRef.Hash() == remoteRef.Hash() {
		return nil
	}

	// Find merge base
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return fmt.Errorf("failed to get repo path: %w", err)
	}
	mergeBase, err := getMergeBase(ctx, repoPath, localRef.Hash().String(), remoteRef.Hash().String())
	if err != nil {
		return fmt.Errorf("failed to find merge base: %w", err)
	}

	// If local is ancestor of remote (merge base == local), fast-forward to remote
	if mergeBase == localRef.Hash() {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to fast-forward branch ref: %w", err)
		}
		if usedTempRef {
			_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
		}
		return nil
	}

	// Collect commits reachable from local but not from remote and cherry-pick
	// them onto the remote tip. This preserves local-only commits even when the
	// local metadata branch already contains old merge commits, while avoiding
	// replaying shared ancestors older than the true merge-base.
	localCommits, err := collectCommitsSince(ctx, repo, repoPath, localRef.Hash(), remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to collect local commits: %w", err)
	}

	if len(localCommits) == 0 {
		// No local-only commits — just point to remote
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to update branch ref: %w", err)
		}
		if usedTempRef {
			_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
		}
		return nil
	}

	newTip, err := cherryPickOnto(ctx, repo, remoteRef.Hash(), localCommits)
	if err != nil {
		return fmt.Errorf("failed to rebase local commits onto remote: %w", err)
	}

	// Update branch ref
	newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), newTip)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update branch ref: %w", err)
	}

	// Clean up temp ref if we used one (best-effort, not critical if it fails)
	if usedTempRef {
		_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
	}

	return nil
}

// getMergeBase returns the merge base hash of two commits, or an error if they
// have no common ancestor.
func getMergeBase(ctx context.Context, repoPath, hashA, hashB string) (plumbing.Hash, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "merge-base", hashA, hashB)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git merge-base failed: %w", err)
	}

	return plumbing.NewHash(strings.TrimSpace(string(output))), nil
}

// collectCommitsSince returns non-merge commits reachable from tip but not from
// exclude, ordered oldest-first in topological order.
func collectCommitsSince(ctx context.Context, repo *git.Repository, repoPath string, tip, exclude plumbing.Hash) ([]*object.Commit, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// cherryPickOnto computes each commit's delta against its first parent, so
	// replaying merge commits would incorrectly re-apply changes that arrived via
	// non-first-parent history. Limit the replay set to non-merge commits.
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--reverse", "--topo-order", "--no-merges", exclude.String()+".."+tip.String())
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list failed: %w", err)
	}

	lines := strings.Fields(string(output))
	if len(lines) > MaxCommitTraversalDepth {
		return nil, fmt.Errorf("commit chain exceeded %d commits; aborting rebase", MaxCommitTraversalDepth)
	}

	commits := make([]*object.Commit, 0, len(lines))
	for _, line := range lines {
		hash := plumbing.NewHash(line)
		commit, commitErr := repo.CommitObject(hash)
		if commitErr != nil {
			return nil, fmt.Errorf("failed to get commit %s: %w", hash, commitErr)
		}
		if len(commit.ParentHashes) > 1 {
			continue
		}
		commits = append(commits, commit)
	}

	return commits, nil
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
