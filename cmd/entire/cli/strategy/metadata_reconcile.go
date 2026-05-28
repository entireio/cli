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
	remote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/utils/merkletrie"
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
		defer repo.Close()
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
		ref := plumbing.NewHashReference(refName, remoteHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to reset metadata branch to remote: %w", err)
		}
		fmt.Fprintln(w, "[entire] Done — local had no checkpoint data, reset to remote")
		return nil
	}

	fmt.Fprintf(w, "[entire] Cherry-picking %d local checkpoint(s) onto remote...\n", len(dataCommits))

	newTip, err := cherryPickOnto(ctx, repo, remoteHash, dataCommits, shallow)
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

// v2DoctorTmpRef is the temporary ref used by doctor to fetch and compare the remote v2 /main.
// Uses the refs/entire-fetch-tmp/ namespace consistent with checkpoint_remote.go.
const v2DoctorTmpRef = "refs/entire-fetch-tmp/doctor-v2-main"

// IsV2MainDisconnected checks whether the local v2 /main ref and the remote
// v2 /main ref exist but share no common ancestor. Uses git ls-remote to
// discover the remote ref (custom refs don't have remote-tracking refs).
//
// remote is the git remote name, URL, or local path to check against.
// Returns (false, nil) if either ref doesn't exist or they share ancestry.
func IsV2MainDisconnected(ctx context.Context, repo *git.Repository, remote string) (bool, error) {
	refName := plumbing.ReferenceName(paths.V2MainRefName)

	localRef, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check local v2 /main ref: %w", err)
	}

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return false, err
	}

	remoteHash, err := lsRemoteRef(ctx, repoPath, remote, paths.V2MainRefName)
	if err != nil {
		return false, fmt.Errorf("failed to ls-remote v2 /main: %w", err)
	}
	if remoteHash == plumbing.ZeroHash {
		return false, nil // Remote doesn't have the ref
	}

	if localRef.Hash() == remoteHash {
		return false, nil
	}

	// Fetch remote ref to temporary local ref for merge-base check.
	// Use the fetched hash (not ls-remote hash) since the remote may have advanced.
	if fetchErr := fetchRefToTemp(ctx, repoPath, remote, paths.V2MainRefName, v2DoctorTmpRef); fetchErr != nil {
		return false, fmt.Errorf("failed to fetch remote v2 /main: %w", fetchErr)
	}
	defer cleanupTmpRef(repo)

	fetchedHash, err := resolveRefHash(repo, v2DoctorTmpRef)
	if err != nil {
		return false, fmt.Errorf("failed to read fetched v2 /main ref: %w", err)
	}

	if localRef.Hash() == fetchedHash {
		return false, nil
	}

	return isDisconnected(ctx, repoPath, localRef.Hash().String(), fetchedHash.String())
}

// ReconcileDisconnectedV2Ref detects and repairs disconnected local/remote
// v2 /main refs. Same strategy as v1: cherry-pick local commits onto remote tip.
// The remote is discovered via git ls-remote and fetched to a temp ref.
//
// remote is the git remote name, URL, or local path.
func ReconcileDisconnectedV2Ref(
	ctx context.Context,
	repo *git.Repository,
	remote string,
	w io.Writer,
) error {
	refName := plumbing.ReferenceName(paths.V2MainRefName)

	localRef, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check local v2 /main ref: %w", err)
	}

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return err
	}

	remoteHash, err := lsRemoteRef(ctx, repoPath, remote, paths.V2MainRefName)
	if err != nil {
		return fmt.Errorf("failed to ls-remote v2 /main: %w", err)
	}
	if remoteHash == plumbing.ZeroHash {
		return nil
	}

	if localRef.Hash() == remoteHash {
		return nil
	}

	if fetchErr := fetchRefToTemp(ctx, repoPath, remote, paths.V2MainRefName, v2DoctorTmpRef); fetchErr != nil {
		return fmt.Errorf("failed to fetch remote v2 /main: %w", fetchErr)
	}
	defer cleanupTmpRef(repo)

	// Use the fetched hash (not ls-remote hash) since the remote may have advanced.
	fetchedHash, err := resolveRefHash(repo, v2DoctorTmpRef)
	if err != nil {
		return fmt.Errorf("failed to read fetched v2 /main ref: %w", err)
	}

	if localRef.Hash() == fetchedHash {
		return nil
	}

	disconnected, err := isDisconnected(ctx, repoPath, localRef.Hash().String(), fetchedHash.String())
	if err != nil {
		return fmt.Errorf("failed to check v2 /main ancestry: %w", err)
	}
	if !disconnected {
		return nil
	}

	fmt.Fprintln(w, "[entire] Detected disconnected v2 /main refs (local and remote share no common ancestor)")

	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("failed to load shallow boundaries: %w", err)
	}

	localCommits, err := collectCommitChain(repo, localRef.Hash(), shallow)
	if err != nil {
		return fmt.Errorf("failed to collect local commits: %w", err)
	}

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
		ref := plumbing.NewHashReference(refName, fetchedHash)
		if setErr := repo.Storer.SetReference(ref); setErr != nil {
			return fmt.Errorf("failed to reset v2 /main to remote: %w", setErr)
		}
		fmt.Fprintln(w, "[entire] Done — local had no checkpoint data, reset to remote")
		return nil
	}

	fmt.Fprintf(w, "[entire] Cherry-picking %d local checkpoint(s) onto remote...\n", len(dataCommits))

	newTip, err := cherryPickOnto(ctx, repo, fetchedHash, dataCommits, shallow)
	if err != nil {
		return fmt.Errorf("failed to cherry-pick local commits onto remote: %w", err)
	}

	ref := plumbing.NewHashReference(refName, newTip)
	if setErr := repo.Storer.SetReference(ref); setErr != nil {
		return fmt.Errorf("failed to update v2 /main ref: %w", setErr)
	}

	fmt.Fprintln(w, "[entire] Done — all local and remote checkpoints preserved")
	return nil
}

// lsRemoteRef runs git ls-remote and returns the hash for a specific ref.
// Returns plumbing.ZeroHash if the ref doesn't exist on the remote.
func lsRemoteRef(ctx context.Context, repoPath, remoteName, refName string) (plumbing.Hash, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	fetchTarget, err := remote.ResolveFetchTarget(ctx, remoteName)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve fetch target for ls-remote: %w", err)
	}

	output, err := remote.LsRemoteInDir(ctx, repoPath, fetchTarget, refName)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git ls-remote %s failed: %w", remote.RedactURL(fetchTarget), err)
	}

	line := strings.TrimSpace(string(output))
	if line == "" {
		return plumbing.ZeroHash, nil
	}

	parts := strings.Fields(line)
	if len(parts) < 2 {
		return plumbing.ZeroHash, nil
	}

	return plumbing.NewHash(parts[0]), nil
}

// fetchRefToTemp fetches a remote ref to a temporary local ref for comparison.
func fetchRefToTemp(ctx context.Context, repoPath, remoteName, srcRef, dstRef string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	fetchTarget, err := remote.ResolveFetchTarget(ctx, remoteName)
	if err != nil {
		return fmt.Errorf("resolve fetch target for doctor v2 fetch: %w", err)
	}

	refspec := fmt.Sprintf("+%s:%s", srcRef, dstRef)
	output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:    fetchTarget,
		RefSpecs:  []string{refspec},
		NoTags:    true,
		Unshallow: true,
		Dir:       repoPath,
	})
	if err != nil {
		redactedURL := remote.RedactURL(fetchTarget)
		msg := strings.TrimSpace(strings.ReplaceAll(string(output), fetchTarget, redactedURL))
		if msg != "" {
			return fmt.Errorf("git fetch %s failed: %s: %w", redactedURL, msg, err)
		}
		return fmt.Errorf("git fetch %s failed: %w", redactedURL, err)
	}
	return nil
}

// resolveRefHash reads the commit hash that a ref points to.
func resolveRefHash(repo *git.Repository, refName string) (plumbing.Hash, error) {
	ref, err := repo.Reference(plumbing.ReferenceName(refName), true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("ref %s not found: %w", refName, err)
	}
	return ref.Hash(), nil
}

// cleanupTmpRef deletes the temporary ref used by doctor checks.
func cleanupTmpRef(repo *git.Repository) {
	_ = repo.Storer.RemoveReference(plumbing.ReferenceName(v2DoctorTmpRef)) //nolint:errcheck // best-effort cleanup
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
