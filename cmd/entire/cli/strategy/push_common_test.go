package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
