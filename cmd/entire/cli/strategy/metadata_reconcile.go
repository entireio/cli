package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/utils/merkletrie"
)

// disconnectedOnce ensures the disconnection warning runs at most once per process.
var disconnectedOnce sync.Once //nolint:gochecknoglobals // intentional per-process gate

// MetadataRelation classifies how the local primary metadata ref stands relative
// to a remote-tracking ref. Derived from a single `git merge-base`, so the
// disconnected, fast-forward and diverged verdicts can never disagree — they used
// to be computed by two functions that each shelled out for the same pair.
type MetadataRelation int

const (
	// MetadataRelationAbsent: one of the two refs does not exist, so there is
	// nothing to relate.
	MetadataRelationAbsent MetadataRelation = iota
	// MetadataRelationAligned: identical tips.
	MetadataRelationAligned
	// MetadataRelationLocalAhead: the remote tip is an ancestor of local.
	MetadataRelationLocalAhead
	// MetadataRelationRemoteAhead: local is an ancestor of the remote tip, so a
	// fetch fast-forwards.
	MetadataRelationRemoteAhead
	// MetadataRelationDiverged: both sides advanced past their merge base, so a
	// fetch from the elected remote replays the local commits onto the remote tip.
	MetadataRelationDiverged
	// MetadataRelationDisconnected: no merge base at all (the "empty-orphan bug").
	MetadataRelationDisconnected
)

// MetadataComparison is a MetadataRelation together with the tips it was derived
// from. Local and Remote are zero when Relation is MetadataRelationAbsent.
type MetadataComparison struct {
	Relation MetadataRelation
	Local    plumbing.Hash
	Remote   plumbing.Hash
}

// CompareMetadataWithRemote classifies the local primary metadata ref against
// remoteRefName. Pure read — it never writes a ref, and it runs exactly one
// merge-base subprocess.
func CompareMetadataWithRemote(ctx context.Context, repo *git.Repository, remoteRefName plumbing.ReferenceName) (MetadataComparison, error) {
	var c MetadataComparison

	refs := checkpoint.ResolveRefs(ctx)
	localRef, err := repo.Reference(refs.Primary, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("failed to check local primary metadata ref: %w", err)
	}
	remoteRef, err := repo.Reference(remoteRefName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("failed to check remote metadata ref: %w", err)
	}

	c.Local, c.Remote = localRef.Hash(), remoteRef.Hash()
	if c.Local == c.Remote {
		c.Relation = MetadataRelationAligned
		return c, nil
	}

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return c, err
	}
	mergeBase, err := getMergeBase(ctx, repoPath, c.Local.String(), c.Remote.String())
	switch {
	case errors.Is(err, errNoMergeBase):
		c.Relation = MetadataRelationDisconnected
	case err != nil:
		return c, fmt.Errorf("failed to find merge base for %s: %w", refs.Primary, err)
	case mergeBase == c.Local:
		c.Relation = MetadataRelationRemoteAhead
	case mergeBase == c.Remote:
		c.Relation = MetadataRelationLocalAhead
	default:
		c.Relation = MetadataRelationDiverged
	}
	return c, nil
}

// IsMetadataDisconnected reports whether the local primary metadata ref and the
// provided fetched or remote-tracking ref exist but share no common ancestor.
func IsMetadataDisconnected(ctx context.Context, repo *git.Repository, remoteRefName plumbing.ReferenceName) (bool, error) {
	c, err := CompareMetadataWithRemote(ctx, repo, remoteRefName)
	if err != nil {
		return false, err
	}
	return c.Relation == MetadataRelationDisconnected, nil
}

// FirstReadCandidateTrackingRef returns the remote name and remote-tracking
// ref of the first checkpoint read candidate (elected sync remote first, then
// the legacy origin tier) whose tracking ref for primary exists locally.
// Pure read; ok is false when no candidate has a tracking ref (or the chain
// is empty).
func FirstReadCandidateTrackingRef(ctx context.Context, repo *git.Repository, primary plumbing.ReferenceName) (string, plumbing.ReferenceName, bool) {
	for _, remoteName := range CheckpointReadRemotes(ctx) {
		refName := plumbing.NewRemoteReferenceName(remoteName, primary.Short())
		if _, err := repo.Reference(refName, true); err == nil {
			return remoteName, refName, true
		}
	}
	return "", "", false
}

// WarnIfMetadataDisconnected checks (once per process) whether the metadata
// branch is disconnected and prints a warning to stderr if so. The check runs
// against the first checkpoint read candidate whose remote-tracking ref
// exists (pure read across both tiers).
// It does NOT fix the problem — users are directed to 'entire doctor'.
//
// Takes the caller's context rather than context.Background(): the probe below
// reads git, so a detached context ignored both interrupt cancellation and the
// invocation-scoped remote-read cache, re-shelling out for answers the caller
// already had.
//
// Uses sync.Once, so a transient failure on the first call permanently suppresses
// the warning. This is acceptable because the check is advisory only and
// 'entire doctor' is the authoritative repair path.
func WarnIfMetadataDisconnected(ctx context.Context) {
	disconnectedOnce.Do(func() {
		repo, err := OpenRepository(ctx)
		if err != nil {
			logging.Debug(ctx, "metadata disconnection check: could not open repository",
				slog.String("error", err.Error()))
			return
		}
		defer repo.Close()
		refs := checkpoint.ResolveRefs(ctx)
		if !refs.PrimaryFetchableFromRemote() {
			return // no remote tracks Primary; nothing to disconnect from
		}
		_, remoteRefName, ok := FirstReadCandidateTrackingRef(ctx, repo, refs.Primary)
		if !ok {
			return // no candidate has a tracking ref; nothing to disconnect from
		}
		disconnected, err := IsMetadataDisconnected(ctx, repo, remoteRefName)
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

// ReconcileDisconnectedMetadataRef detects and repairs disconnected local/remote
// metadata refs. Disconnected means no common ancestor, which
// only happens due to the empty-orphan bug. Diverged (shared ancestor) is normal
// and handled by the push path.
//
// Repair strategy: cherry-pick local commits onto remote tip, preserving all data.
// Checkpoint shards use unique paths (<id[:2]>/<id[2:]>/), so cherry-picks always
// apply cleanly.
//
// Progress messages are written to w (typically os.Stderr for hooks or
// cmd.ErrOrStderr() for commands).
// The remote ref can be either a remote-tracking ref or a temporary fetched ref.
func ReconcileDisconnectedMetadataRef(
	ctx context.Context,
	repo *git.Repository,
	localRefName plumbing.ReferenceName,
	remoteRefName plumbing.ReferenceName,
	w io.Writer,
) error {
	advance := func(hash plumbing.Hash) error {
		return setRefHash(repo, localRefName, hash)
	}

	// Check local ref
	localRef, err := repo.Reference(localRefName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // No local ref — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("failed to check local metadata ref: %w", err)
	}

	// Check remote-tracking or fetched ref
	remoteRef, err := repo.Reference(remoteRefName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // No remote ref — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("failed to check remote metadata ref: %w", err)
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
		return fmt.Errorf("failed to check metadata ref ancestry: %w", err)
	}
	if !disconnected {
		// Shared ancestry (diverged or ancestor) — not our problem
		return nil
	}

	// Disconnected — cherry-pick local commits onto remote tip
	fmt.Fprintln(w, "[entire] Detected disconnected session metadata (local and remote share no common ancestor)")

	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("failed to load shallow boundaries: %w", err)
	}

	// Collect local commits oldest-first
	localCommits, err := collectCommitChain(repo, localHash, shallow)
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
		if err := advance(remoteHash); err != nil {
			return fmt.Errorf("failed to reset metadata ref to remote: %w", err)
		}
		fmt.Fprintln(w, "[entire] Done — local had no checkpoint data, reset to remote")
		return nil
	}

	fmt.Fprintf(w, "[entire] Cherry-picking %d local checkpoint(s) onto remote...\n", len(dataCommits))

	newTip, err := cherryPickOnto(ctx, repo, remoteHash, dataCommits, shallow)
	if err != nil {
		return fmt.Errorf("failed to cherry-pick local commits onto remote: %w", err)
	}

	if err := advance(newTip); err != nil {
		return fmt.Errorf("failed to update metadata ref: %w", err)
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
// Commits listed in shallow are treated as roots — the walk stops at them without
// traversing into their parents. go-git's repo.CommitObject().ParentHashes does not
// consult .git/shallow on its own, so without this check the walk would stroll past
// shallow boundaries into stale objects left in the pack (e.g., when the remote
// branch has been rebuilt since the last full fetch), producing a phantom chain of
// commits that no longer represent the actual checkpoint history.
func collectCommitChain(repo *git.Repository, tip plumbing.Hash, shallow map[plumbing.Hash]bool) ([]*object.Commit, error) {
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
		if shallow[current] {
			// Shallow boundary — treat as a root.
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

// loadShallowHashes returns the commit hashes listed in the repository's
// shallow file, or an empty map if the repository is not shallow.
func loadShallowHashes(ctx context.Context, repoPath string) (map[plumbing.Hash]bool, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoPath, gitDir)
	}
	// Path is constructed from git's own --git-common-dir output, not user input.
	data, err := os.ReadFile(filepath.Join(gitDir, "shallow")) //nolint:gosec // see comment above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[plumbing.Hash]bool{}, nil
		}
		return nil, fmt.Errorf("read shallow file: %w", err)
	}
	set := map[plumbing.Hash]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[plumbing.NewHash(line)] = true
	}
	return set, nil
}

// cherryPickOnto applies each commit's delta onto base, building a linear chain.
// For each commit, it computes the full diff from its parent (additions, modifications,
// and deletions), then applies that delta onto the current tip's tree.
//
// Commits listed in shallow are treated as roots: their delta is computed against
// an empty tree rather than against their (past-the-boundary) parent. Without this,
// a shallow-boundary commit would be diffed against a stale parent tree whose
// objects live in the local pack but no longer represent the actual checkpoint
// history — producing nonsense changes when replayed onto the remote tip.
func cherryPickOnto(ctx context.Context, repo *git.Repository, base plumbing.Hash, commits []*object.Commit, shallow map[plumbing.Hash]bool) (plumbing.Hash, error) {
	currentTip := base

	for _, commit := range commits {
		changes, err := treeChangesForCherryPick(ctx, repo, commit, shallow)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if len(changes) == 0 {
			continue // Skip no-op commits
		}

		tipCommit, err := repo.CommitObject(currentTip)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to get tip commit: %w", err)
		}

		mergedTreeHash, err := checkpoint.ApplyTreeChanges(ctx, repo, tipCommit.TreeHash, changes)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to apply cherry-pick changes: %w", err)
		}

		// Create new commit on top of current tip, preserving original message/author
		newHash, err := createCherryPickCommit(ctx, repo, mergedTreeHash, currentTip, commit)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to create cherry-pick commit: %w", err)
		}

		currentTip = newHash
	}

	return currentTip, nil
}

func treeChangesForCherryPick(ctx context.Context, repo *git.Repository, commit *object.Commit, shallow map[plumbing.Hash]bool) ([]checkpoint.TreeChange, error) {
	commitTree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree for commit %s: %w", commit.Hash, err)
	}

	var parentTree *object.Tree
	// Shallow-boundary commits are treated as roots — see cherryPickOnto for why.
	if len(commit.ParentHashes) > 0 && !shallow[commit.Hash] {
		parentCommit, pErr := repo.CommitObject(commit.ParentHashes[0])
		if pErr != nil {
			return nil, fmt.Errorf("failed to get parent commit %s: %w", commit.ParentHashes[0], pErr)
		}
		parentTree, err = parentCommit.Tree()
		if err != nil {
			return nil, fmt.Errorf("failed to get parent tree for commit %s: %w", commit.ParentHashes[0], err)
		}
	}

	changes, err := object.DiffTreeContext(ctx, parentTree, commitTree)
	if err != nil {
		return nil, fmt.Errorf("failed to diff commit %s against parent: %w", commit.Hash, err)
	}

	treeChanges := make([]checkpoint.TreeChange, 0, len(changes))
	for _, change := range changes {
		treeChange, changeErr := changeToTreeChange(change)
		if changeErr != nil {
			return nil, fmt.Errorf("failed to convert change in commit %s: %w", commit.Hash, changeErr)
		}
		treeChanges = append(treeChanges, treeChange)
	}
	return treeChanges, nil
}

func changeToTreeChange(change *object.Change) (checkpoint.TreeChange, error) {
	action, err := change.Action()
	if err != nil {
		return checkpoint.TreeChange{}, fmt.Errorf("change action: %w", err)
	}

	switch action {
	case merkletrie.Insert, merkletrie.Modify:
		entry := change.To.TreeEntry
		return checkpoint.TreeChange{
			Path: change.To.Name,
			Entry: &object.TreeEntry{
				Name: entry.Name,
				Mode: entry.Mode,
				Hash: entry.Hash,
			},
		}, nil
	case merkletrie.Delete:
		return checkpoint.TreeChange{Path: change.From.Name}, nil
	default:
		return checkpoint.TreeChange{}, fmt.Errorf("unsupported action %s", action)
	}
}

// createCherryPickCommit creates a new commit on top of parent, preserving the
// original commit's message and author.
func createCherryPickCommit(ctx context.Context, repo *git.Repository, treeHash, parent plumbing.Hash, original *object.Commit) (plumbing.Hash, error) {
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

	checkpoint.SignCommitBestEffort(ctx, commit)

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
	return wt.Filesystem().Root(), nil
}
