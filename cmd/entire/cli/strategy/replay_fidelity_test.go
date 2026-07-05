package strategy

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitsFromHashes resolves each hash to its commit object, preserving order.
func commitsFromHashes(t *testing.T, repo *git.Repository, hashes ...plumbing.Hash) []*object.Commit {
	t.Helper()
	commits := make([]*object.Commit, 0, len(hashes))
	for _, h := range hashes {
		c, err := repo.CommitObject(h)
		require.NoError(t, err)
		commits = append(commits, c)
	}
	return commits
}

// D4(a) — regression guard for 743c43f4c ("skip non-root no-op commits in push
// signing"): a no-op commit (identical tree to its parent) must NOT reuse its
// own tree when replayed onto a remote tip that has accumulated a different
// tree. The buggy behaviour reset the tip's tree to the no-op commit's own
// tree, silently dropping files another clone contributed to the remote.
// cherryPickOnto skips no-op commits entirely, so the remote tip's tree is
// preserved. This pins that behaviour on the current replay path (cherryPickOnto),
// which is where the "same skip" the fix referenced actually lives.
func TestCherryPickOnto_NoOpCommitDoesNotClobberRemoteTree(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	ctx := context.Background()

	// Remote tip carries a file contributed by another clone.
	remoteTip := makeTreeCommit(t, repo, nil, "remote-only work", map[string]string{"remote.txt": "R"})

	// Local chain: base -> c1 (adds local.txt) -> c2 (no-op: identical tree to c1).
	base := makeTreeCommit(t, repo, nil, "base", map[string]string{"base.txt": "B"})
	c1 := makeTreeCommit(t, repo, []plumbing.Hash{base}, "add local", map[string]string{"base.txt": "B", "local.txt": "L"})
	c2 := makeTreeCommit(t, repo, []plumbing.Hash{c1}, "no-op child", map[string]string{"base.txt": "B", "local.txt": "L"})

	newTip, err := cherryPickOnto(ctx, repo, remoteTip, commitsFromHashes(t, repo, c1, c2), nil)
	require.NoError(t, err)

	// The remote's accumulated file must survive — the core of the regression.
	assertCommitFile(t, repo, newTip, "remote.txt", "R")
	// c1's real change is replayed on top.
	assertCommitFile(t, repo, newTip, "local.txt", "L")

	// Only c1 produced a commit; the no-op c2 was skipped, so the new tip parents
	// directly onto the remote tip (guards against a spurious clobbering commit).
	newTipCommit, err := repo.CommitObject(newTip)
	require.NoError(t, err)
	require.Len(t, newTipCommit.ParentHashes, 1)
	assert.Equal(t, remoteTip, newTipCommit.ParentHashes[0],
		"no-op commit must not add a second (tree-clobbering) commit on top of the remote tip")
}

// D4(b) — root-commit replay: a local chain whose oldest commit is a true root
// (no parents) replays cleanly onto a remote tip. The root's delta is computed
// against the empty tree (parentTree nil in treeChangesForCherryPick), so its
// files are added rather than diffed against a phantom parent.
func TestCherryPickOnto_RootCommitChainReplays(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	ctx := context.Background()

	remoteTip := makeTreeCommit(t, repo, nil, "remote-only work", map[string]string{"remote.txt": "R"})

	// Local chain rooted at a parentless commit.
	localRoot := makeTreeCommit(t, repo, nil, "local root", map[string]string{"root.txt": "root"})
	child := makeTreeCommit(t, repo, []plumbing.Hash{localRoot}, "local child", map[string]string{"root.txt": "root", "child.txt": "child"})

	newTip, err := cherryPickOnto(ctx, repo, remoteTip, commitsFromHashes(t, repo, localRoot, child), nil)
	require.NoError(t, err)

	assertCommitFile(t, repo, newTip, "remote.txt", "R")
	assertCommitFile(t, repo, newTip, "root.txt", "root")
	assertCommitFile(t, repo, newTip, "child.txt", "child")

	// Both local commits were applied (root produced content), so the chain above
	// the remote tip is exactly two commits.
	tip, err := repo.CommitObject(newTip)
	require.NoError(t, err)
	require.Len(t, tip.ParentHashes, 1)
	mid, err := repo.CommitObject(tip.ParentHashes[0])
	require.NoError(t, err)
	require.Len(t, mid.ParentHashes, 1)
	assert.Equal(t, remoteTip, mid.ParentHashes[0])
}

// D4(c) — KNOWN BUG PIN for 4cf01edb3 ("Drop commit-count cap when replaying
// commits during pre-push").
//
// The pre-push diverged-replay path (fetchAndRebaseRefCommon -> collectCommitsSince)
// aborts once the local-only commit set exceeds MaxCommitTraversalDepth (1000).
// A checkpoint branch can legitimately accumulate more local-only commits than
// that, so the cap turns a valid push into a hard failure. Commit 4cf01edb3
// removes the cap, but it lives on the unmerged branch origin/no-limit and is NOT
// an ancestor of this branch (verified: `git merge-base --is-ancestor 4cf01edb3
// HEAD` -> false), so the cap is still active here.
//
// This test PINS the current (capped) behaviour. When the cap-removal lands,
// flip it: replace require.Error with require.NoError and assert all commits are
// returned. The sibling collectCommitChain cap is already pinned by
// TestCollectCommitChain_DepthLimit.
func TestCollectCommitsSince_CommitCapPinsCurrentBehavior(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// A single empty tree, reused by every synthetic commit — keeps the build fast.
	emptyTree := &object.Tree{}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, emptyTree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	// base (excluded) followed by MaxCommitTraversalDepth+1 single-parent commits,
	// so rev-list base..tip yields one more entry than the cap allows.
	var base, tip plumbing.Hash
	for i := range MaxCommitTraversalDepth + 2 {
		c := &object.Commit{
			TreeHash:  treeHash,
			Author:    object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(int64(i), 0).UTC()},
			Committer: object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(int64(i), 0).UTC()},
			Message:   "commit\n",
		}
		if tip != plumbing.ZeroHash {
			c.ParentHashes = []plumbing.Hash{tip}
		}
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, c.Encode(obj))
		h, sErr := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, sErr)
		if i == 0 {
			base = h
		}
		tip = h
	}

	_, err = collectCommitsSince(context.Background(), repo, dir, tip, base)
	require.Error(t, err, "KNOWN BUG (4cf01edb3, unmerged): >1000-commit replay is capped and errors")
	assert.Contains(t, err.Error(), "exceeded")
	assert.Contains(t, err.Error(), "aborting rebase")
}

// D5 — regression guard for 1e8628ade ("Use fetched ref hash instead of
// ls-remote hash for disconnect check"). The remote may advance between an
// earlier hash observation and the fetch; reconcile/replay must operate on the
// hash the fetch actually landed, not a stale earlier one.
//
// The literal ls-remote-before-fetch seam that 1e8628ade patched no longer
// exists in the refactored code (the fix is an ancestor of this branch:
// `git merge-base --is-ancestor 1e8628ade HEAD` -> true; the reconcile path now
// always reads the post-fetch remote-tracking ref). So this pins the behavioural
// invariant end-to-end: fetchAndRebaseRefCommon replays local-only commits onto
// the freshly fetched remote tip. We advance the remote AFTER the local repo
// last saw it, then reconcile, and prove the new local tip descends from the
// advanced (fetched) remote commit and carries its content — not the stale one.
//
// Not parallel: fetchAndRebaseRefCommon opens the repository from the working
// directory, so the test uses t.Chdir.
func TestFetchAndRebaseRefCommon_UsesFetchedRemoteHash(t *testing.T) {
	ctx := context.Background()
	v1Ref := plumbing.NewBranchReferenceName(paths.MetadataBranchName)

	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "README.md", "# work")
	testutil.GitAdd(t, workDir, "README.md")
	testutil.GitCommit(t, workDir, "init")

	runGit := func(dir string, args ...string) string {
		t.Helper()
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		c.Env = testutil.GitIsolatedEnv()
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
		return strings.TrimSpace(string(out))
	}

	// Build a v1 metadata branch with a base checkpoint shard, as an orphan.
	runGit(workDir, "checkout", "--orphan", paths.MetadataBranchName)
	runGit(workDir, "rm", "-rf", ".")
	testutil.WriteFile(t, workDir, "aa/aaaaaaaaaa/metadata.json", `{"checkpoint_id":"aaaaaaaaaaaa"}`)
	runGit(workDir, "add", ".")
	runGit(workDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	baseHash := plumbing.NewHash(runGit(workDir, "rev-parse", "HEAD"))

	// Bare remote seeded with the base v1.
	bareDir := t.TempDir()
	runGit(bareDir, "init", "--bare")
	runGit(workDir, "remote", "add", "origin", bareDir)
	runGit(workDir, "push", "origin", paths.MetadataBranchName)

	// Another clone advances the remote v1 to R2 (a new checkpoint shard), so the
	// remote tip is now ahead of anything workDir has observed.
	scratch := t.TempDir()
	runGit(scratch, "clone", bareDir, ".")
	runGit(scratch, "config", "user.email", "s@example.com")
	runGit(scratch, "config", "user.name", "Scratch")
	runGit(scratch, "config", "commit.gpgsign", "false")
	runGit(scratch, "checkout", paths.MetadataBranchName)
	testutil.WriteFile(t, scratch, "bb/bbbbbbbbbb/metadata.json", `{"checkpoint_id":"bbbbbbbbbbbb"}`)
	runGit(scratch, "add", ".")
	runGit(scratch, "commit", "-m", "Checkpoint: bbbbbbbbbbbb")
	runGit(scratch, "push", "origin", paths.MetadataBranchName)
	remoteAdvancedHash := plumbing.NewHash(runGit(scratch, "rev-parse", "HEAD"))
	require.NotEqual(t, baseHash, remoteAdvancedHash)

	// workDir diverges locally from base WITHOUT fetching the advance: reset v1 to
	// base, add a local-only checkpoint. workDir has never seen R2.
	runGit(workDir, "checkout", paths.MetadataBranchName)
	testutil.WriteFile(t, workDir, "cc/cccccccccc/metadata.json", `{"checkpoint_id":"cccccccccccc"}`)
	runGit(workDir, "add", ".")
	runGit(workDir, "commit", "-m", "Checkpoint: cccccccccccc")

	t.Chdir(workDir)

	require.NoError(t, fetchAndRebaseRefCommon(ctx, "origin", v1Ref))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	localRef, err := repo.Reference(v1Ref, true)
	require.NoError(t, err)

	// The reconciled local tip must descend from the FETCHED remote tip (R2), not
	// the stale base — proving reconcile used the fetched hash.
	require.True(t, IsAncestorOf(ctx, repo, remoteAdvancedHash, localRef.Hash()),
		"reconciled local v1 must build on the fetched remote tip (R2), not a stale hash")

	// Both the remote-only checkpoint (from R2) and the local-only checkpoint survive.
	assertCommitFile(t, repo, localRef.Hash(), "bb/bbbbbbbbbb/metadata.json", `{"checkpoint_id":"bbbbbbbbbbbb"}`)
	assertCommitFile(t, repo, localRef.Hash(), "cc/cccccccccc/metadata.json", `{"checkpoint_id":"cccccccccccc"}`)
}
