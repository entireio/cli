package strategy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasUnpushedSessionsCommon(t *testing.T) {
	t.Parallel()

	branchName := "entire/checkpoints/v1"

	setupRepo := func(t *testing.T) (*git.Repository, plumbing.Hash) {
		t.Helper()
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		repo, err := git.PlainOpen(tmpDir)
		require.NoError(t, err)

		head, err := repo.Head()
		require.NoError(t, err)

		localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), head.Hash())
		require.NoError(t, repo.Storer.SetReference(localRef))

		return repo, head.Hash()
	}

	t.Run("no remote tracking ref exists", func(t *testing.T) {
		t.Parallel()
		repo, headHash := setupRepo(t)
		assert.True(t, hasUnpushedSessionsCommon(repo, "origin", headHash, branchName))
	})

	t.Run("local and remote same hash", func(t *testing.T) {
		t.Parallel()
		repo, headHash := setupRepo(t)

		remoteRef := plumbing.NewHashReference(
			plumbing.NewRemoteReferenceName("origin", branchName),
			headHash,
		)
		require.NoError(t, repo.Storer.SetReference(remoteRef))

		assert.False(t, hasUnpushedSessionsCommon(repo, "origin", headHash, branchName))
	})

	t.Run("local differs from remote", func(t *testing.T) {
		t.Parallel()
		repo, _ := setupRepo(t)

		differentHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		assert.True(t, hasUnpushedSessionsCommon(repo, "origin", differentHash, branchName))
	})
}

// setupRepoWithCheckpointBranch creates a temp repo with one commit and a local
// entire/checkpoints/v1 branch pointing at HEAD. Returns the repo directory.
// Caller must call t.Chdir(tmpDir) if needed (not done here to keep the helper composable).
func setupRepoWithCheckpointBranch(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)

	localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), head.Hash())
	require.NoError(t, repo.Storer.SetReference(localRef))

	return tmpDir
}

// TestDoPushBranch_UnreachableTarget_ReturnsNil exercises the graceful degradation
// path in doPushBranch: when the push target is unreachable, the function logs a
// warning and returns nil (no error). This is the core behavior that ensures a
// failing checkpoint remote never blocks the user's main push.
//
// Not parallel: uses t.Chdir() (required for OpenRepository in fetchAndMergeSessionsCommon).
func TestDoPushBranch_UnreachableTarget_ReturnsNil(t *testing.T) {
	tmpDir := setupRepoWithCheckpointBranch(t)
	t.Chdir(tmpDir)

	ctx := context.Background()

	// Use a non-existent path as the push target. doPushBranch will:
	// 1. Try to push (fails — target doesn't exist)
	// 2. Try to fetch+merge (fails — can't fetch from non-existent path)
	// 3. Log warning and return nil (graceful degradation)
	nonExistentPath := filepath.Join(t.TempDir(), "does-not-exist")
	err := doPushBranch(ctx, nonExistentPath, paths.MetadataBranchName)
	assert.NoError(t, err, "doPushBranch should return nil when target is unreachable (graceful degradation)")
}

// TestPushBranchIfNeeded_UnreachableTarget_ReturnsNil exercises the full push path
// through pushBranchIfNeeded with an unreachable local path target. This verifies
// that the complete production code path (branch existence check -> push attempt ->
// graceful failure) works end-to-end.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestPushBranchIfNeeded_UnreachableTarget_ReturnsNil(t *testing.T) {
	tmpDir := setupRepoWithCheckpointBranch(t)
	t.Chdir(tmpDir)

	ctx := context.Background()

	// Push to a non-existent path. pushBranchIfNeeded will:
	// 1. Open repository (CWD-based)
	// 2. Verify branch exists locally
	// 3. Since target is not a URL (no :// or @), check hasUnpushedSessionsCommon
	//    which finds no remote tracking ref -> returns true (has unpushed)
	// 4. Call doPushBranch which fails gracefully
	nonExistentPath := filepath.Join(t.TempDir(), "does-not-exist")
	err := pushBranchIfNeeded(ctx, nonExistentPath, paths.MetadataBranchName)
	assert.NoError(t, err, "pushBranchIfNeeded should return nil when target is unreachable")
}

// TestPushBranchIfNeeded_LocalBareRepo_PushesSuccessfully verifies that
// pushBranchIfNeeded works with a local bare repo path as the target.
// This exercises the same code path that PrePush uses when pushTarget()
// returns a URL, but with a local path. It validates the core routing
// behavior: a branch can be pushed to an arbitrary target path.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestPushBranchIfNeeded_LocalBareRepo_PushesSuccessfully(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithCheckpointBranch(t)

	// Create a bare repo as the push target.
	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	if output, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare failed: %v\n%s", err, output)
	}

	t.Chdir(tmpDir)

	// Push using pushBranchIfNeeded with the bare repo path as target.
	err := pushBranchIfNeeded(ctx, bareDir, paths.MetadataBranchName)
	require.NoError(t, err, "pushBranchIfNeeded should succeed with a local bare repo target")

	// Verify the branch arrived on the bare repo.
	verifyCmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+paths.MetadataBranchName)
	verifyCmd.Dir = bareDir
	verifyCmd.Env = testutil.GitIsolatedEnv()
	if output, err := verifyCmd.CombinedOutput(); err != nil {
		t.Errorf("branch should exist on bare remote after push: %v\n%s", err, output)
	}
}

// TestFetchAndRebase_DivergedBranches verifies that when local and remote
// metadata branches have diverged (shared ancestor, different commits on each),
// fetchAndRebaseSessionsCommon produces a linear history (no merge commits)
// with all data from both sides preserved.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_DivergedBranches(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	// 1. Create bare origin with a metadata branch containing a base checkpoint
	bareDir := t.TempDir()
	workDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	// Init bare + push initial main commit
	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(workDir, "clone", bareDir, ".")
	gitRun(workDir, "config", "user.email", "test@test.com")
	gitRun(workDir, "config", "user.name", "Test User")
	gitRun(workDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(workDir, "add", ".")
	gitRun(workDir, "commit", "-m", "init")
	gitRun(workDir, "push", "origin", "main")

	// Create orphan metadata branch with a base checkpoint, push to origin
	gitRun(workDir, "checkout", "--orphan", branchName)
	gitRun(workDir, "rm", "-rf", ".")
	baseDir := filepath.Join(workDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(workDir, "add", ".")
	gitRun(workDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(workDir, "push", "origin", branchName)
	gitRun(workDir, "checkout", "main")

	// 2. Clone into two separate working directories
	cloneA := filepath.Join(t.TempDir(), "cloneA")
	cloneB := filepath.Join(t.TempDir(), "cloneB")
	require.NoError(t, os.MkdirAll(cloneA, 0o755))
	require.NoError(t, os.MkdirAll(cloneB, 0o755))

	gitRun(cloneA, "clone", bareDir, ".")
	gitRun(cloneA, "config", "user.email", "a@test.com")
	gitRun(cloneA, "config", "user.name", "User A")
	gitRun(cloneA, "config", "commit.gpgsign", "false")

	gitRun(cloneB, "clone", bareDir, ".")
	gitRun(cloneB, "config", "user.email", "b@test.com")
	gitRun(cloneB, "config", "user.name", "User B")
	gitRun(cloneB, "config", "commit.gpgsign", "false")

	// Both clones create local metadata branches tracking origin
	for _, dir := range []string{cloneA, cloneB} {
		gitRun(dir, "branch", branchName, "origin/"+branchName)
	}

	// 3. Add a local-only checkpoint on clone A
	gitRun(cloneA, "checkout", branchName)
	localDir := filepath.Join(cloneA, "bb", "bbbbbbbbbb")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"bbbbbbbbbbbb"}`), 0o644))
	gitRun(cloneA, "add", ".")
	gitRun(cloneA, "commit", "-m", "Checkpoint: bbbbbbbbbbbb")
	gitRun(cloneA, "checkout", "main")

	// 4. Add a remote-only checkpoint via clone B and push it
	gitRun(cloneB, "checkout", branchName)
	remoteDir := filepath.Join(cloneB, "cc", "cccccccccc")
	require.NoError(t, os.MkdirAll(remoteDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remoteDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"cccccccccccc"}`), 0o644))
	gitRun(cloneB, "add", ".")
	gitRun(cloneB, "commit", "-m", "Checkpoint: cccccccccccc")
	gitRun(cloneB, "push", "origin", branchName)
	gitRun(cloneB, "checkout", "main")

	// 5. Run fetchAndRebaseSessionsCommon on clone A (diverged: local has bb, remote has cc)
	t.Chdir(cloneA)

	err := fetchAndRebaseSessionsCommon(ctx, "origin", branchName)
	require.NoError(t, err)

	// 6. Verify results
	repo, err := git.PlainOpen(cloneA)
	require.NoError(t, err)

	refName := plumbing.NewBranchReferenceName(branchName)
	localRef, err := repo.Reference(refName, true)
	require.NoError(t, err)

	// Walk history and verify it's fully linear (no merge commits)
	current := localRef.Hash()
	for range 10 {
		c, cErr := repo.CommitObject(current)
		require.NoError(t, cErr)
		assert.LessOrEqual(t, len(c.ParentHashes), 1, "expected linear history, commit %s has %d parents", c.Hash, len(c.ParentHashes))
		if len(c.ParentHashes) == 0 {
			break
		}
		current = c.ParentHashes[0]
	}

	// Verify the final tree contains all three checkpoints
	tipCommit, err := repo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))

	assert.Contains(t, entries, "aa/aaaaaaaaaa/metadata.json", "base checkpoint should be preserved")
	assert.Contains(t, entries, "bb/bbbbbbbbbb/metadata.json", "local checkpoint should be preserved")
	assert.Contains(t, entries, "cc/cccccccccc/metadata.json", "remote checkpoint should be preserved")
}

// TestFetchAndRebase_LocalBehind verifies that when local is an ancestor of remote,
// fetchAndRebaseSessionsCommon fast-forwards.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_LocalBehind(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	bareDir := t.TempDir()
	workDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(workDir, "clone", bareDir, ".")
	gitRun(workDir, "config", "user.email", "test@test.com")
	gitRun(workDir, "config", "user.name", "Test User")
	gitRun(workDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(workDir, "add", ".")
	gitRun(workDir, "commit", "-m", "init")
	gitRun(workDir, "push", "origin", "main")

	// Create metadata branch with base commit
	gitRun(workDir, "checkout", "--orphan", branchName)
	gitRun(workDir, "rm", "-rf", ".")
	baseDir := filepath.Join(workDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(workDir, "add", ".")
	gitRun(workDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(workDir, "push", "origin", branchName)
	gitRun(workDir, "checkout", "main")

	// Clone
	cloneDir := filepath.Join(t.TempDir(), "clone")
	require.NoError(t, os.MkdirAll(cloneDir, 0o755))
	gitRun(cloneDir, "clone", bareDir, ".")
	gitRun(cloneDir, "config", "user.email", "test@test.com")
	gitRun(cloneDir, "config", "user.name", "Test User")
	gitRun(cloneDir, "config", "commit.gpgsign", "false")
	gitRun(cloneDir, "branch", branchName, "origin/"+branchName)

	// Add another commit on origin via workDir
	gitRun(workDir, "checkout", branchName)
	remoteDir := filepath.Join(workDir, "bb", "bbbbbbbbbb")
	require.NoError(t, os.MkdirAll(remoteDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remoteDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"bbbbbbbbbbbb"}`), 0o644))
	gitRun(workDir, "add", ".")
	gitRun(workDir, "commit", "-m", "Checkpoint: bbbbbbbbbbbb")
	gitRun(workDir, "push", "origin", branchName)
	gitRun(workDir, "checkout", "main")

	// Clone is now behind — fetchAndRebase should fast-forward
	t.Chdir(cloneDir)

	err := fetchAndRebaseSessionsCommon(ctx, "origin", branchName)
	require.NoError(t, err)

	// Verify local now matches remote
	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	require.NoError(t, err)

	assert.Equal(t, remoteRef.Hash(), localRef.Hash(), "local should fast-forward to remote")
}

// TestFetchAndRebase_MergeBaseOnSecondParent_DoesNotReplayAncestors verifies
// that rebasing a metadata branch with an existing merge commit does not replay
// ancestors older than the true merge-base. Replaying those ancestors can
// resurrect checkpoint shards that the remote deleted after the merge-base.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_MergeBaseOnSecondParent_DoesNotReplayAncestors(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	bareDir := t.TempDir()
	setupDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	// Initialize origin and seed main.
	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(setupDir, "clone", bareDir, ".")
	gitRun(setupDir, "config", "user.email", "test@test.com")
	gitRun(setupDir, "config", "user.name", "Test User")
	gitRun(setupDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "init")
	gitRun(setupDir, "push", "origin", "main")

	// Seed metadata branch with checkpoint aa.
	gitRun(setupDir, "checkout", "--orphan", branchName)
	gitRun(setupDir, "rm", "-rf", ".")
	baseDir := filepath.Join(setupDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(setupDir, "push", "origin", branchName)
	gitRun(setupDir, "checkout", "main")

	// Clone twice: local gets the old merge-commit history, remote advances later.
	cloneLocal := filepath.Join(t.TempDir(), "clone-local")
	cloneRemote := filepath.Join(t.TempDir(), "clone-remote")
	require.NoError(t, os.MkdirAll(cloneLocal, 0o755))
	require.NoError(t, os.MkdirAll(cloneRemote, 0o755))

	for _, dir := range []string{cloneLocal, cloneRemote} {
		gitRun(dir, "clone", bareDir, ".")
		gitRun(dir, "config", "user.email", "test@test.com")
		gitRun(dir, "config", "user.name", "Test User")
		gitRun(dir, "config", "commit.gpgsign", "false")
		gitRun(dir, "branch", branchName, "origin/"+branchName)
	}

	// Local commit B: add checkpoint bb.
	gitRun(cloneLocal, "checkout", branchName)
	localDir := filepath.Join(cloneLocal, "bb", "bbbbbbbbbb")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"bbbbbbbbbbbb"}`), 0o644))
	gitRun(cloneLocal, "add", ".")
	gitRun(cloneLocal, "commit", "-m", "Checkpoint: bbbbbbbbbbbb")

	// Remote commit C: add checkpoint cc and push.
	gitRun(cloneRemote, "checkout", branchName)
	remoteDirC := filepath.Join(cloneRemote, "cc", "cccccccccc")
	require.NoError(t, os.MkdirAll(remoteDirC, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remoteDirC, "metadata.json"),
		[]byte(`{"checkpoint_id":"cccccccccccc"}`), 0o644))
	gitRun(cloneRemote, "add", ".")
	gitRun(cloneRemote, "commit", "-m", "Checkpoint: cccccccccccc")
	gitRun(cloneRemote, "push", "origin", branchName)

	// Local old-style sync: fetch and merge origin/metadata, creating a merge commit.
	gitRun(cloneLocal, "fetch", "origin", branchName)
	gitRun(cloneLocal, "merge", "--no-ff", "--no-edit", "origin/"+branchName)

	// Remote commit D: delete checkpoint aa after C and push.
	require.NoError(t, os.Remove(filepath.Join(cloneRemote, "aa", "aaaaaaaaaa", "metadata.json")))
	gitRun(cloneRemote, "add", "-A")
	remoteDirD := filepath.Join(cloneRemote, "dd", "dddddddddd")
	require.NoError(t, os.MkdirAll(remoteDirD, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remoteDirD, "metadata.json"),
		[]byte(`{"checkpoint_id":"dddddddddddd"}`), 0o644))
	gitRun(cloneRemote, "add", ".")
	gitRun(cloneRemote, "commit", "-m", "Checkpoint: dddddddddddd")
	gitRun(cloneRemote, "push", "origin", branchName)
	gitRun(cloneRemote, "checkout", "main")
	gitRun(cloneLocal, "checkout", "main")

	// Rebase local metadata branch onto the updated remote tip.
	t.Chdir(cloneLocal)

	err := fetchAndRebaseSessionsCommon(ctx, "origin", branchName)
	require.NoError(t, err)

	repo, err := git.PlainOpen(cloneLocal)
	require.NoError(t, err)

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)

	tipCommit, err := repo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))

	current := localRef.Hash()
	for range 10 {
		c, cErr := repo.CommitObject(current)
		require.NoError(t, cErr)
		assert.LessOrEqual(t, len(c.ParentHashes), 1, "replayed history should stay linear, commit %s has %d parents", c.Hash, len(c.ParentHashes))
		if len(c.ParentHashes) == 0 {
			break
		}
		current = c.ParentHashes[0]
	}

	assert.NotContains(t, entries, "aa/aaaaaaaaaa/metadata.json",
		"rebasing should not replay ancestors older than the true merge-base")
	assert.Contains(t, entries, "bb/bbbbbbbbbb/metadata.json", "local checkpoint should be preserved")
	assert.Contains(t, entries, "cc/cccccccccc/metadata.json", "merged remote checkpoint should be preserved")
	assert.Contains(t, entries, "dd/dddddddddd/metadata.json", "new remote checkpoint should be preserved")
}

// TestFetchAndRebase_DoesNotResurrectRemoteOnlyCheckpointFromMerge verifies that
// replaying a local merge commit does not resurrect a checkpoint that only ever
// existed on the remote side of that merge and was later deleted remotely.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_DoesNotResurrectRemoteOnlyCheckpointFromMerge(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	bareDir := t.TempDir()
	setupDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(setupDir, "clone", bareDir, ".")
	gitRun(setupDir, "config", "user.email", "test@test.com")
	gitRun(setupDir, "config", "user.name", "Test User")
	gitRun(setupDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "init")
	gitRun(setupDir, "push", "origin", "main")

	gitRun(setupDir, "checkout", "--orphan", branchName)
	gitRun(setupDir, "rm", "-rf", ".")
	baseDir := filepath.Join(setupDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(setupDir, "push", "origin", branchName)
	gitRun(setupDir, "checkout", "main")

	cloneLocal := filepath.Join(t.TempDir(), "clone-local")
	cloneRemote := filepath.Join(t.TempDir(), "clone-remote")
	require.NoError(t, os.MkdirAll(cloneLocal, 0o755))
	require.NoError(t, os.MkdirAll(cloneRemote, 0o755))

	for _, dir := range []string{cloneLocal, cloneRemote} {
		gitRun(dir, "clone", bareDir, ".")
		gitRun(dir, "config", "user.email", "test@test.com")
		gitRun(dir, "config", "user.name", "Test User")
		gitRun(dir, "config", "commit.gpgsign", "false")
		gitRun(dir, "branch", branchName, "origin/"+branchName)
	}

	// Local-only checkpoint B.
	gitRun(cloneLocal, "checkout", branchName)
	localDir := filepath.Join(cloneLocal, "bb", "bbbbbbbbbb")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"bbbbbbbbbbbb"}`), 0o644))
	gitRun(cloneLocal, "add", ".")
	gitRun(cloneLocal, "commit", "-m", "Checkpoint: bbbbbbbbbbbb")

	// Remote-only checkpoint E that will later be deleted remotely.
	gitRun(cloneRemote, "checkout", branchName)
	remoteOnlyDir := filepath.Join(cloneRemote, "ee", "eeeeeeeeee")
	require.NoError(t, os.MkdirAll(remoteOnlyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remoteOnlyDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"eeeeeeeeeeee"}`), 0o644))
	gitRun(cloneRemote, "add", ".")
	gitRun(cloneRemote, "commit", "-m", "Checkpoint: eeeeeeeeeeee")
	gitRun(cloneRemote, "push", "origin", branchName)

	// Local old-style sync creates merge M that brings in remote-only checkpoint E.
	gitRun(cloneLocal, "fetch", "origin", branchName)
	gitRun(cloneLocal, "merge", "--no-ff", "--no-edit", "origin/"+branchName)

	// Remote deletes E and adds D.
	require.NoError(t, os.Remove(filepath.Join(cloneRemote, "ee", "eeeeeeeeee", "metadata.json")))
	gitRun(cloneRemote, "add", "-A")
	remoteDirD := filepath.Join(cloneRemote, "dd", "dddddddddd")
	require.NoError(t, os.MkdirAll(remoteDirD, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remoteDirD, "metadata.json"),
		[]byte(`{"checkpoint_id":"dddddddddddd"}`), 0o644))
	gitRun(cloneRemote, "add", ".")
	gitRun(cloneRemote, "commit", "-m", "Checkpoint: dddddddddddd")
	gitRun(cloneRemote, "push", "origin", branchName)
	gitRun(cloneRemote, "checkout", "main")
	gitRun(cloneLocal, "checkout", "main")

	t.Chdir(cloneLocal)

	err := fetchAndRebaseSessionsCommon(ctx, "origin", branchName)
	require.NoError(t, err)

	repo, err := git.PlainOpen(cloneLocal)
	require.NoError(t, err)

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)

	tipCommit, err := repo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))

	assert.Contains(t, entries, "bb/bbbbbbbbbb/metadata.json", "local checkpoint should be preserved")
	assert.Contains(t, entries, "dd/dddddddddd/metadata.json", "new remote checkpoint should be preserved")
	assert.NotContains(t, entries, "ee/eeeeeeeeee/metadata.json",
		"replaying the local merge should not resurrect a remote-only checkpoint deleted later on the remote")
}

// TestFetchAndRebase_NonOriginRemote_ReconcilesFetchedRef verifies that
// fetchAndRebaseSessionsCommon reconciles against the remote that was actually
// fetched instead of assuming origin.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_NonOriginRemote_ReconcilesFetchedRef(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	bareDir := t.TempDir()
	setupDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(setupDir, "clone", bareDir, ".")
	gitRun(setupDir, "config", "user.email", "test@test.com")
	gitRun(setupDir, "config", "user.name", "Test User")
	gitRun(setupDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "init")
	gitRun(setupDir, "push", "origin", "main")

	gitRun(setupDir, "checkout", "--orphan", branchName)
	gitRun(setupDir, "rm", "-rf", ".")
	baseDir := filepath.Join(setupDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(setupDir, "push", "origin", branchName)
	gitRun(setupDir, "checkout", "main")

	cloneDir := filepath.Join(t.TempDir(), "clone")
	require.NoError(t, os.MkdirAll(cloneDir, 0o755))
	gitRun(cloneDir, "clone", bareDir, ".")
	gitRun(cloneDir, "config", "user.email", "test@test.com")
	gitRun(cloneDir, "config", "user.name", "Test User")
	gitRun(cloneDir, "config", "commit.gpgsign", "false")
	gitRun(cloneDir, "remote", "rename", "origin", "backup")
	gitRun(cloneDir, "branch", branchName, "backup/"+branchName)

	// Replace local metadata with a disconnected orphan commit.
	gitRun(cloneDir, "checkout", "--orphan", "temp-orphan")
	gitRun(cloneDir, "rm", "-rf", ".")
	localDir := filepath.Join(cloneDir, "cc", "cccccccccc")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"cccccccccccc"}`), 0o644))
	gitRun(cloneDir, "add", ".")
	gitRun(cloneDir, "commit", "-m", "Checkpoint: cccccccccccc")
	gitRun(cloneDir, "branch", "-f", branchName, "temp-orphan")
	gitRun(cloneDir, "checkout", "main")

	// Create stale origin tracking data that must be ignored by reconciliation.
	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	localRefBeforeFetch, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)
	staleOriginRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", branchName),
		localRefBeforeFetch.Hash(),
	)
	require.NoError(t, repo.Storer.SetReference(staleOriginRef))

	t.Chdir(cloneDir)

	err = fetchAndRebaseSessionsCommon(ctx, "backup", branchName)
	require.NoError(t, err)

	repo, err = git.PlainOpen(cloneDir)
	require.NoError(t, err)

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)
	backupRef, err := repo.Reference(plumbing.NewRemoteReferenceName("backup", branchName), true)
	require.NoError(t, err)

	tipCommit, err := repo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	require.Len(t, tipCommit.ParentHashes, 1)
	assert.Equal(t, backupRef.Hash(), tipCommit.ParentHashes[0], "reconciliation should use the fetched remote tip")

	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))
	assert.Contains(t, entries, "aa/aaaaaaaaaa/metadata.json", "remote checkpoint should be preserved")
	assert.Contains(t, entries, "cc/cccccccccc/metadata.json", "local checkpoint should be preserved")
}

// TestFetchAndRebase_URLTarget_ReconcilesFetchedTempRef verifies that URL
// targets reconcile against the temporary fetched ref instead of any origin
// tracking state.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_URLTarget_ReconcilesFetchedTempRef(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	bareDir := t.TempDir()
	setupDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(setupDir, "clone", bareDir, ".")
	gitRun(setupDir, "config", "user.email", "test@test.com")
	gitRun(setupDir, "config", "user.name", "Test User")
	gitRun(setupDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "init")
	gitRun(setupDir, "push", "origin", "main")

	gitRun(setupDir, "checkout", "--orphan", branchName)
	gitRun(setupDir, "rm", "-rf", ".")
	baseDir := filepath.Join(setupDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(setupDir, "push", "origin", branchName)
	gitRun(setupDir, "checkout", "main")

	cloneDir := filepath.Join(t.TempDir(), "clone")
	require.NoError(t, os.MkdirAll(cloneDir, 0o755))
	gitRun(cloneDir, "clone", bareDir, ".")
	gitRun(cloneDir, "config", "user.email", "test@test.com")
	gitRun(cloneDir, "config", "user.name", "Test User")
	gitRun(cloneDir, "config", "commit.gpgsign", "false")
	gitRun(cloneDir, "branch", branchName, "origin/"+branchName)

	gitRun(cloneDir, "checkout", "--orphan", "temp-orphan")
	gitRun(cloneDir, "rm", "-rf", ".")
	localDir := filepath.Join(cloneDir, "cc", "cccccccccc")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"cccccccccccc"}`), 0o644))
	gitRun(cloneDir, "add", ".")
	gitRun(cloneDir, "commit", "-m", "Checkpoint: cccccccccccc")
	gitRun(cloneDir, "branch", "-f", branchName, "temp-orphan")
	gitRun(cloneDir, "checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	localRefBeforeFetch, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)
	staleOriginRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", branchName),
		localRefBeforeFetch.Hash(),
	)
	require.NoError(t, repo.Storer.SetReference(staleOriginRef))

	t.Chdir(cloneDir)

	err = fetchAndRebaseSessionsCommon(ctx, "file://"+bareDir, branchName)
	require.NoError(t, err)

	repo, err = git.PlainOpen(cloneDir)
	require.NoError(t, err)

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)

	tipCommit, err := repo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	require.Len(t, tipCommit.ParentHashes, 1)

	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))
	assert.Contains(t, entries, "aa/aaaaaaaaaa/metadata.json", "remote checkpoint should be preserved")
	assert.Contains(t, entries, "cc/cccccccccc/metadata.json", "local checkpoint should be preserved")

	_, err = repo.Reference(plumbing.ReferenceName("refs/entire-fetch-tmp/"+branchName), true)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "temporary fetched ref should be cleaned up")
}

// TestFetchAndRebase_FlaggedOriginTarget_UsesTempRef verifies that enabling
// filtered_fetches for a normal remote-name target follows the temp-ref
// path and still cleans up after rebasing.
//
// Not parallel: uses t.Chdir() (required for OpenRepository).
func TestFetchAndRebase_FlaggedOriginTarget_UsesTempRef(t *testing.T) {
	ctx := context.Background()
	branchName := paths.MetadataBranchName

	bareDir := t.TempDir()
	setupDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
	}

	gitRun(bareDir, "init", "--bare", "-b", "main")
	gitRun(setupDir, "clone", bareDir, ".")
	gitRun(setupDir, "config", "user.email", "test@test.com")
	gitRun(setupDir, "config", "user.name", "Test User")
	gitRun(setupDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test"), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "init")
	gitRun(setupDir, "push", "origin", "main")

	gitRun(setupDir, "checkout", "--orphan", branchName)
	gitRun(setupDir, "rm", "-rf", ".")
	baseDir := filepath.Join(setupDir, "aa", "aaaaaaaaaa")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa"}`), 0o644))
	gitRun(setupDir, "add", ".")
	gitRun(setupDir, "commit", "-m", "Checkpoint: aaaaaaaaaaaa")
	gitRun(setupDir, "push", "origin", branchName)
	gitRun(setupDir, "checkout", "main")

	cloneDir := filepath.Join(t.TempDir(), "clone")
	require.NoError(t, os.MkdirAll(cloneDir, 0o755))
	gitRun(cloneDir, "clone", bareDir, ".")
	gitRun(cloneDir, "config", "user.email", "test@test.com")
	gitRun(cloneDir, "config", "user.name", "Test User")
	gitRun(cloneDir, "config", "commit.gpgsign", "false")
	gitRun(cloneDir, "branch", branchName, "origin/"+branchName)
	require.NoError(t, os.MkdirAll(filepath.Join(cloneDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cloneDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`),
		0o644,
	))

	gitRun(cloneDir, "checkout", "--orphan", "temp-orphan")
	gitRun(cloneDir, "rm", "-rf", ".")
	localDir := filepath.Join(cloneDir, "cc", "cccccccccc")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "metadata.json"),
		[]byte(`{"checkpoint_id":"cccccccccccc"}`), 0o644))
	gitRun(cloneDir, "add", ".")
	gitRun(cloneDir, "commit", "-m", "Checkpoint: cccccccccccc")
	gitRun(cloneDir, "branch", "-f", branchName, "temp-orphan")
	gitRun(cloneDir, "checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	localRefBeforeFetch, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)
	staleOriginRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", branchName),
		localRefBeforeFetch.Hash(),
	)
	require.NoError(t, repo.Storer.SetReference(staleOriginRef))

	t.Chdir(cloneDir)

	err = fetchAndRebaseSessionsCommon(ctx, "origin", branchName)
	require.NoError(t, err)

	repo, err = git.PlainOpen(cloneDir)
	require.NoError(t, err)

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	require.NoError(t, err)

	tipCommit, err := repo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	require.Len(t, tipCommit.ParentHashes, 1)

	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))
	assert.Contains(t, entries, "aa/aaaaaaaaaa/metadata.json", "remote checkpoint should be preserved")
	assert.Contains(t, entries, "cc/cccccccccc/metadata.json", "local checkpoint should be preserved")

	_, err = repo.Reference(plumbing.ReferenceName("refs/entire-fetch-tmp/"+branchName), true)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "temporary fetched ref should be cleaned up")
}

// TestIsCheckpointRemoteCommitted verifies that the discoverability check reads
// the committed content of .entire/settings.json at HEAD, not just tracking status.
// Not parallel: uses t.Chdir().
func TestIsCheckpointRemoteCommitted(t *testing.T) {
	checkpointRemoteSettings := `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`

	t.Run("false when settings.json not committed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Create .entire/settings.json with checkpoint_remote but don't commit it
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"),
			[]byte(checkpointRemoteSettings), 0o644))

		t.Chdir(tmpDir)
		assert.False(t, isCheckpointRemoteCommitted(context.Background()))
	})

	t.Run("false when committed settings.json has no checkpoint_remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Commit settings.json without checkpoint_remote
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{}`), 0o644))
		testutil.GitAdd(t, tmpDir, ".entire/settings.json")
		testutil.GitCommit(t, tmpDir, "add settings")

		t.Chdir(tmpDir)
		assert.False(t, isCheckpointRemoteCommitted(context.Background()))
	})

	t.Run("true when committed settings.json has checkpoint_remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Commit settings.json with checkpoint_remote
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"),
			[]byte(checkpointRemoteSettings), 0o644))
		testutil.GitAdd(t, tmpDir, ".entire/settings.json")
		testutil.GitCommit(t, tmpDir, "add settings")

		t.Chdir(tmpDir)
		assert.True(t, isCheckpointRemoteCommitted(context.Background()))
	})

	t.Run("false when checkpoint_remote only in local changes", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Commit settings.json without checkpoint_remote
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{}`), 0o644))
		testutil.GitAdd(t, tmpDir, ".entire/settings.json")
		testutil.GitCommit(t, tmpDir, "add settings without remote")

		// Now add checkpoint_remote locally but don't commit
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"),
			[]byte(checkpointRemoteSettings), 0o644))

		t.Chdir(tmpDir)
		assert.False(t, isCheckpointRemoteCommitted(context.Background()),
			"uncommitted checkpoint_remote should not count as discoverable")
	})

	t.Run("works from subdirectory", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"),
			[]byte(checkpointRemoteSettings), 0o644))
		testutil.GitAdd(t, tmpDir, ".entire/settings.json")
		testutil.GitCommit(t, tmpDir, "add settings")

		subDir := filepath.Join(tmpDir, "subdir")
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		t.Chdir(subDir)
		assert.True(t, isCheckpointRemoteCommitted(context.Background()),
			"should detect committed checkpoint_remote from subdirectory")
	})
}

// TestPrintSettingsCommitHint verifies the hint only prints for URL targets
// when checkpoint_remote is not discoverable from committed settings, and only
// once per process via sync.Once.
// Not parallel: uses t.Chdir() and resets package-level settingsHintOnce.
func TestPrintSettingsCommitHint(t *testing.T) {
	checkpointRemoteSettings := `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`

	t.Run("no hint for non-URL target", func(t *testing.T) {
		settingsHintOnce = sync.Once{}

		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")
		t.Chdir(tmpDir)

		old := os.Stderr
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = w

		printSettingsCommitHint(context.Background(), "origin")

		w.Close()
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(r); readErr != nil {
			t.Fatalf("read pipe: %v", readErr)
		}
		os.Stderr = old

		assert.Empty(t, buf.String(), "should not print hint for non-URL target")
	})

	t.Run("hint when checkpoint_remote not in committed settings", func(t *testing.T) {
		settingsHintOnce = sync.Once{}

		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Create .entire/settings.json but don't commit it
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"),
			[]byte(checkpointRemoteSettings), 0o644))
		t.Chdir(tmpDir)

		old := os.Stderr
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = w

		printSettingsCommitHint(context.Background(), "git@github.com:org/repo.git")

		w.Close()
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(r); readErr != nil {
			t.Fatalf("read pipe: %v", readErr)
		}
		os.Stderr = old

		assert.Contains(t, buf.String(), "does not contain checkpoint_remote")
		assert.Contains(t, buf.String(), "entire.io will not be able to discover")
	})

	t.Run("hint when committed settings lacks checkpoint_remote", func(t *testing.T) {
		settingsHintOnce = sync.Once{}

		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Commit settings.json without checkpoint_remote
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{}`), 0o644))
		testutil.GitAdd(t, tmpDir, ".entire/settings.json")
		testutil.GitCommit(t, tmpDir, "add settings")
		t.Chdir(tmpDir)

		old := os.Stderr
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = w

		printSettingsCommitHint(context.Background(), "git@github.com:org/repo.git")

		w.Close()
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(r); readErr != nil {
			t.Fatalf("read pipe: %v", readErr)
		}
		os.Stderr = old

		assert.Contains(t, buf.String(), "does not contain checkpoint_remote",
			"should warn when committed settings.json exists but lacks checkpoint_remote")
	})

	t.Run("no hint when checkpoint_remote is committed", func(t *testing.T) {
		settingsHintOnce = sync.Once{}

		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		// Commit settings.json with checkpoint_remote
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"),
			[]byte(checkpointRemoteSettings), 0o644))
		testutil.GitAdd(t, tmpDir, ".entire/settings.json")
		testutil.GitCommit(t, tmpDir, "add settings with checkpoint remote")
		t.Chdir(tmpDir)

		old := os.Stderr
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = w

		printSettingsCommitHint(context.Background(), "git@github.com:org/repo.git")

		w.Close()
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(r); readErr != nil {
			t.Fatalf("read pipe: %v", readErr)
		}
		os.Stderr = old

		assert.Empty(t, buf.String(), "should not print hint when checkpoint_remote is committed")
	})

	t.Run("prints only once per process", func(t *testing.T) {
		settingsHintOnce = sync.Once{}

		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")
		t.Chdir(tmpDir)

		old := os.Stderr
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = w

		// Call twice — should only print once
		printSettingsCommitHint(context.Background(), "git@github.com:org/repo.git")
		printSettingsCommitHint(context.Background(), "git@github.com:org/repo.git")

		w.Close()
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(r); readErr != nil {
			t.Fatalf("read pipe: %v", readErr)
		}
		os.Stderr = old

		count := bytes.Count(buf.Bytes(), []byte("does not contain checkpoint_remote"))
		assert.Equal(t, 1, count, "hint should print exactly once, got %d", count)
	})
}

// captureStderr redirects os.Stderr to a pipe and returns a function that restores
// stderr and returns the captured output. Must be called on the main goroutine
// (not parallel-safe). Uses t.Cleanup as a safety net to restore stderr and close
// pipe file descriptors if the test fails or panics before the returned function
// is called.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w

	// Safety net: restore stderr and close pipe ends on test failure/panic.
	// In the normal path the returned function handles cleanup first;
	// duplicate Close calls return an error that we intentionally ignore.
	t.Cleanup(func() {
		os.Stderr = old
		_ = w.Close()
		_ = r.Close()
	})

	return func() string {
		_ = w.Close()
		var buf bytes.Buffer
		_, readErr := buf.ReadFrom(r)
		require.NoError(t, readErr)
		_ = r.Close()
		os.Stderr = old
		return buf.String()
	}
}

// setupBareRemoteWithCheckpointBranch creates a work repo with a checkpoint branch
// and a bare remote that already has the branch pushed. Returns (workDir, bareDir).
// Caller must t.Chdir(workDir) before calling push functions.
func setupBareRemoteWithCheckpointBranch(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	workDir := setupRepoWithCheckpointBranch(t)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	// Push the checkpoint branch to the bare remote
	pushCmd := exec.CommandContext(ctx, "git", "push", bareDir, paths.MetadataBranchName)
	pushCmd.Dir = workDir
	pushCmd.Env = testutil.GitIsolatedEnv()
	out, err = pushCmd.CombinedOutput()
	require.NoError(t, err, "initial push failed: %s", out)

	return workDir, bareDir
}

// TestDoPushBranch_AlreadyUpToDate verifies that when the remote already has all
// commits, the output says "already up-to-date" instead of "done".
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestDoPushBranch_AlreadyUpToDate(t *testing.T) {
	workDir, bareDir := setupBareRemoteWithCheckpointBranch(t)
	t.Chdir(workDir)

	restore := captureStderr(t)
	err := doPushBranch(context.Background(), bareDir, paths.MetadataBranchName)
	output := restore()

	require.NoError(t, err)
	assert.Contains(t, output, "already up-to-date", "should indicate nothing was pushed")
	assert.NotContains(t, output, " done", "should not say 'done' when nothing was pushed")
}

// TestDoPushBranch_NewContent_SaysDone verifies that when there are new commits
// to push, the output says "done".
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestDoPushBranch_NewContent_SaysDone(t *testing.T) {
	workDir := setupRepoWithCheckpointBranch(t)

	// Create a bare remote with no checkpoint branch yet
	bareDir := t.TempDir()
	initCmd := exec.CommandContext(context.Background(), "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	t.Chdir(workDir)

	restore := captureStderr(t)
	err = doPushBranch(context.Background(), bareDir, paths.MetadataBranchName)
	output := restore()

	require.NoError(t, err)
	assert.Contains(t, output, " done", "should say 'done' when new content was pushed")
	assert.NotContains(t, output, "already up-to-date", "should not say 'already up-to-date' when content was pushed")
}
