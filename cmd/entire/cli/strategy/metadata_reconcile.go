package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// disconnectedOnce ensures the disconnection warning runs at most once per process.
var disconnectedOnce sync.Once //nolint:gochecknoglobals // intentional per-process gate

// IsMetadataDisconnected checks whether the local metadata branch
// and the provided fetched or remote-tracking ref exist but share no common
// ancestor.
func IsMetadataDisconnected(ctx context.Context, repo *git.Repository, remoteRefName plumbing.ReferenceName) (bool, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	localRef, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check local metadata branch: %w", err)
	}

	remoteRef, err := repo.Reference(remoteRefName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check remote metadata branch: %w", err)
	}

	if localRef.Hash() == remoteRef.Hash() {
		return false, nil
	}

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return false, err
	}

	return isDisconnected(ctx, repoPath, localRef.Hash().String(), remoteRef.Hash().String())
}

// WarnIfMetadataDisconnected checks (once per process) whether the metadata
// branch is disconnected and prints a warning to stderr if so.
// It does NOT fix the problem — users are directed to 'entire doctor'.
//
// Uses sync.Once, so a transient failure on the first call permanently suppresses
// the warning. This is acceptable because the check is advisory only and
// 'entire doctor' is the authoritative repair path.
func WarnIfMetadataDisconnected() {
	disconnectedOnce.Do(func() {
		ctx := context.Background()
		repo, err := OpenRepository(ctx)
		if err != nil {
			logging.Debug(ctx, "metadata disconnection check: could not open repository",
				slog.String("error", err.Error()))
			return
		}
		disconnected, err := IsMetadataDisconnected(ctx, repo, plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName))
		if err != nil {
			logging.Debug(ctx, "metadata disconnection check failed",
				slog.String("error", err.Error()))
			return
		}
		if !disconnected {
			return
		}
		fmt.Fprintln(os.Stderr, "[entire] Warning: Local and remote session metadata branches are disconnected.")
		fmt.Fprintln(os.Stderr, "[entire] Some checkpoints from remote may not be visible. Run 'entire doctor' to fix.")
	})
}

// ReconcileDisconnectedMetadataBranch detects and repairs disconnected local/remote
// entire/checkpoints/v1 branches. Disconnected means no common ancestor, which
// only happens due to the empty-orphan bug. Diverged (shared ancestor) is normal
// and handled by the push path's tree merge.
//
// Repair strategy: cherry-pick local commits onto remote tip, preserving all data.
// Checkpoint shards use unique paths (<id[:2]>/<id[2:]>/), so cherry-picks always
// apply cleanly.
//
// Progress messages are written to w (typically os.Stderr for hooks or
// cmd.ErrOrStderr() for commands).
// The remote ref can be either a remote-tracking ref or a temporary fetched ref.
func ReconcileDisconnectedMetadataBranch(
	ctx context.Context,
	repo *git.Repository,
	remoteRefName plumbing.ReferenceName,
	w io.Writer,
) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)

	// Check local branch
	localRef, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // No local branch — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("failed to check local metadata branch: %w", err)
	}

	// Check remote-tracking branch
	remoteRef, err := repo.Reference(remoteRefName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // No remote branch — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("failed to check remote metadata branch: %w", err)
	}

	localHash := localRef.Hash()
	remoteHash := remoteRef.Hash()

	// Same hash — nothing to do
	if localHash == remoteHash {
		return nil
	}

	// Check if disconnected using git merge-base
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return err
	}

	disconnected, err := isDisconnected(ctx, repoPath, localHash.String(), remoteHash.String())
	if err != nil {
		return fmt.Errorf("failed to check metadata branch ancestry: %w", err)
	}
	if !disconnected {
		// Shared ancestry (diverged or ancestor) — not our problem
		return nil
	}

	// Disconnected — cherry-pick local commits onto remote tip
	fmt.Fprintln(w, "[entire] Detected disconnected session metadata (local and remote share no common ancestor)")

	// Collect local commits oldest-first
	localCommits, err := collectCommitChain(repo, localHash)
	if err != nil {
		return fmt.Errorf("failed to collect local commits: %w", err)
	}

	// Filter out empty-tree commits (the orphan bug commit)
	var dataCommits []*object.Commit
	for _, c := range localCommits {
		tree, treeErr := c.Tree()
		if treeErr != nil {
			return fmt.Errorf("failed to read tree for commit %s: %w", c.Hash.String()[:7], treeErr)
		}
		if len(tree.Entries) > 0 {
			dataCommits = append(dataCommits, c)
		}
	}

	if len(dataCommits) == 0 {
		// Local only had empty orphan — just point to remote
		ref := plumbing.NewHashReference(refName, remoteHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to reset metadata branch to remote: %w", err)
		}
		fmt.Fprintln(w, "[entire] Done — local had no checkpoint data, reset to remote")
		return nil
	}

	fmt.Fprintf(w, "[entire] Cherry-picking %d local checkpoint(s) onto remote...\n", len(dataCommits))

	newTip, err := cherryPickOnto(repo, remoteHash, dataCommits)
	if err != nil {
		return fmt.Errorf("failed to cherry-pick local commits onto remote: %w", err)
	}

	// Update local branch ref
	ref := plumbing.NewHashReference(refName, newTip)
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update metadata branch: %w", err)
	}

	fmt.Fprintln(w, "[entire] Done — all local and remote checkpoints preserved")
	return nil
}

// isDisconnected checks if two commits have no common ancestor using git merge-base.
// Returns (true, nil) if disconnected, (false, nil) if they share ancestry,
// or (false, error) if git merge-base failed for another reason.
//
// git merge-base exit codes:
//   - 0: common ancestor found (shared ancestry)
//   - 1: no common ancestor (disconnected)
//   - 128+: error (corrupt repo, invalid hash, etc.)
func isDisconnected(ctx context.Context, repoPath, hashA, hashB string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "merge-base", hashA, hashB)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true, nil // No common ancestor — disconnected
		}
		return false, fmt.Errorf("git merge-base failed: %w", err)
	}
	return false, nil // Shared ancestry
}

// collectCommitChain walks from tip to root following first parent, returns oldest-first.
func collectCommitChain(repo *git.Repository, tip plumbing.Hash) ([]*object.Commit, error) {
	var chain []*object.Commit
	current := tip

	reachedRoot := false
	for range MaxCommitTraversalDepth {
		commit, err := repo.CommitObject(current)
		if err != nil {
			return nil, fmt.Errorf("failed to get commit %s: %w", current, err)
		}
		chain = append(chain, commit)

		if len(commit.ParentHashes) == 0 {
			reachedRoot = true
			break
		}
		current = commit.ParentHashes[0]
	}

	if !reachedRoot {
		return nil, fmt.Errorf("commit chain exceeded %d commits without reaching root; aborting reconciliation", MaxCommitTraversalDepth)
	}

	// Reverse to oldest-first
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	return chain, nil
}

// cherryPickOnto applies each commit's delta onto base, building a linear chain.
// For each commit, it computes the full diff from its parent (additions, modifications,
// and deletions), then applies that delta onto the current tip's tree.
func cherryPickOnto(repo *git.Repository, base plumbing.Hash, commits []*object.Commit) (plumbing.Hash, error) {
	currentTip := base

	for _, commit := range commits {
		// Get the commit's tree entries
		commitTree, err := commit.Tree()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get tree for commit %s: %w", commit.Hash, err)
		}

		commitEntries := make(map[string]object.TreeEntry)
		if err := checkpoint.FlattenTree(repo, commitTree, "", commitEntries); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to flatten commit tree: %w", err)
		}

		// Get parent's tree entries (empty if root commit)
		parentEntries := make(map[string]object.TreeEntry)
		if len(commit.ParentHashes) > 0 {
			parentCommit, pErr := repo.CommitObject(commit.ParentHashes[0])
			if pErr != nil {
				return plumbing.ZeroHash, fmt.Errorf("failed to get parent commit %s: %w", commit.ParentHashes[0], pErr)
			}
			parentTree, ptErr := parentCommit.Tree()
			if ptErr != nil {
				return plumbing.ZeroHash, fmt.Errorf("failed to get parent tree for commit %s: %w", commit.ParentHashes[0], ptErr)
			}
			if err := checkpoint.FlattenTree(repo, parentTree, "", parentEntries); err != nil {
				return plumbing.ZeroHash, fmt.Errorf("failed to flatten parent tree for commit %s: %w", commit.ParentHashes[0], err)
			}
		}

		// Compute full delta: additions, modifications, and deletions
		added := make(map[string]object.TreeEntry)
		for path, entry := range commitEntries {
			parentEntry, exists := parentEntries[path]
			if !exists || parentEntry.Hash != entry.Hash {
				added[path] = entry // New or modified
			}
		}
		var deleted []string
		for path := range parentEntries {
			if _, exists := commitEntries[path]; !exists {
				deleted = append(deleted, path) // Removed in this commit
			}
		}

		if len(added) == 0 && len(deleted) == 0 {
			continue // Skip no-op commits
		}

		// Get current tip's tree and apply delta
		tipCommit, err := repo.CommitObject(currentTip)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get tip commit: %w", err)
		}
		tipTree, err := tipCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get tip tree: %w", err)
		}

		mergedEntries := make(map[string]object.TreeEntry)
		if err := checkpoint.FlattenTree(repo, tipTree, "", mergedEntries); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to flatten tip tree: %w", err)
		}
		for path, entry := range added {
			mergedEntries[path] = entry
		}
		for _, path := range deleted {
			delete(mergedEntries, path)
		}

		mergedTreeHash, err := checkpoint.BuildTreeFromEntries(repo, mergedEntries)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to build merged tree: %w", err)
		}

		// Create new commit on top of current tip, preserving original message/author
		newHash, err := createCherryPickCommit(repo, mergedTreeHash, currentTip, commit)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to create cherry-pick commit: %w", err)
		}

		currentTip = newHash
	}

	return currentTip, nil
}

// createCherryPickCommit creates a new commit on top of parent, preserving the
// original commit's message and author.
func createCherryPickCommit(repo *git.Repository, treeHash, parent plumbing.Hash, original *object.Commit) (plumbing.Hash, error) {
	committerName, committerEmail := GetGitAuthorFromRepo(repo)
	now := time.Now()

	commit := &object.Commit{
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{parent},
		Author:       original.Author,
		Committer: object.Signature{
			Name:  committerName,
			Email: committerEmail,
			When:  now,
		},
		Message: original.Message,
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

// getRepoPath returns the filesystem path for the repository's worktree.
func getRepoPath(repo *git.Repository) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree: %w", err)
	}
	return wt.Filesystem.Root(), nil
}
