package strategy

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupRepoWithV2Ref creates a temp repo with one commit and a v2 /main ref.
// Returns the repo directory.
func setupRepoWithV2Ref(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Create v2 /main ref with an empty tree
	emptyTree, err := checkpoint.BuildTreeFromEntries(repo, map[string]object.TreeEntry{})
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(repo, emptyTree, plumbing.ZeroHash,
		"Init v2 main", "Test", "test@test.com")
	require.NoError(t, err)

	ref := plumbing.NewHashReference(plumbing.ReferenceName(paths.V2MainRefName), commitHash)
	require.NoError(t, repo.Storer.SetReference(ref))

	return tmpDir
}

// Not parallel: uses t.Chdir()
func TestPushRefIfNeeded_NoLocalRef_ReturnsNil(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()
	err := pushRefIfNeeded(ctx, "origin", plumbing.ReferenceName(paths.V2MainRefName))
	assert.NoError(t, err)
}

// Not parallel: uses t.Chdir()
func TestPushRefIfNeeded_LocalBareRepo_PushesSuccessfully(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithV2Ref(t)
	t.Chdir(tmpDir)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	err := pushRefIfNeeded(ctx, bareDir, plumbing.ReferenceName(paths.V2MainRefName))
	require.NoError(t, err)

	// Verify ref exists in bare repo
	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	assert.NoError(t, err, "v2 /main ref should exist in bare repo after push")
}

// Not parallel: uses t.Chdir()
func TestPushRefIfNeeded_UnreachableTarget_ReturnsNil(t *testing.T) {
	tmpDir := setupRepoWithV2Ref(t)
	t.Chdir(tmpDir)

	ctx := context.Background()
	nonExistentPath := filepath.Join(t.TempDir(), "does-not-exist")
	err := pushRefIfNeeded(ctx, nonExistentPath, plumbing.ReferenceName(paths.V2MainRefName))
	assert.NoError(t, err, "pushRefIfNeeded should return nil when target is unreachable")
}

func TestShortRefName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{"refs/entire/checkpoints/v2/main", "v2/main"},
		{"refs/entire/checkpoints/v2/full/current", "v2/full/current"},
		{"refs/entire/checkpoints/v2/full/0000000000001", "v2/full/0000000000001"},
		{"refs/heads/main", "refs/heads/main"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, shortRefName(plumbing.ReferenceName(tt.input)))
		})
	}
}

// Not parallel: uses t.Chdir()
func TestFetchV2MainRefIfMissing_SkipsWhenExists(t *testing.T) {
	tmpDir := setupRepoWithV2Ref(t)
	t.Chdir(tmpDir)

	ctx := context.Background()
	// Should be a no-op since the ref already exists locally
	err := fetchV2MainRefIfMissing(ctx, "https://example.com/repo.git")
	assert.NoError(t, err)
}

// writeV2Checkpoint writes a checkpoint to both /main and /full/current via V2GitStore.
func writeV2Checkpoint(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionID string) {
	t.Helper()
	store := checkpoint.NewV2GitStore(repo, "origin")
	err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"from":"` + sessionID + `"}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
}

// TestFetchAndMergeRef_MergesTrees verifies that fetchAndMergeRef correctly
// merges divergent trees from two repos sharing a common ref.
// Not parallel: uses t.Chdir()
func TestFetchAndMergeRef_MergesTrees(t *testing.T) {
	ctx := context.Background()
	refName := plumbing.ReferenceName(paths.V2MainRefName)

	// Create source repo with a v2 /main ref containing one checkpoint
	srcDir := setupRepoWithV2Ref(t)
	srcRepo, err := git.PlainOpen(srcDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, srcRepo, id.MustCheckpointID("aabbccddeeff"), "session-src")

	// Create a bare "remote" and push src to it
	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	pushCmd := exec.CommandContext(ctx, "git", "push", bareDir,
		string(refName)+":"+string(refName))
	pushCmd.Dir = srcDir
	require.NoError(t, pushCmd.Run())

	// Create a local repo that also has the ref but with a different checkpoint
	localDir := setupRepoWithV2Ref(t)
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, localRepo, id.MustCheckpointID("112233445566"), "session-local")

	t.Chdir(localDir)

	// Fetch and merge — should combine both checkpoints
	err = fetchAndMergeRef(ctx, bareDir, refName)
	require.NoError(t, err)

	// Verify merged tree contains both checkpoints on /main
	mergedRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	ref, err := mergedRepo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := mergedRepo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(mergedRepo, tree, "", entries))

	// Should have entries from both checkpoints (aa/ shard and 11/ shard)
	hasAA := false
	has11 := false
	for path := range entries {
		if strings.HasPrefix(path, "aa/") {
			hasAA = true
		}
		if strings.HasPrefix(path, "11/") {
			has11 = true
		}
	}
	assert.True(t, hasAA, "merged tree should contain checkpoint aabbccddeeff")
	assert.True(t, has11, "merged tree should contain checkpoint 112233445566")
}

// TestPushV2Refs_PushesAllRefs verifies that pushV2Refs pushes /main,
// /full/current, and any archived generations to a bare repo.
// Not parallel: uses t.Chdir()
func TestPushV2Refs_PushesAllRefs(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithV2Ref(t)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Write a checkpoint (creates both /main and /full/current)
	writeV2Checkpoint(t, repo, id.MustCheckpointID("aabbccddeeff"), "test-session")

	// Create two fake archived generation refs — only the latest should be pushed
	fullRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)
	for _, num := range []string{"0000000000001", "0000000000002"} {
		ref := plumbing.NewHashReference(
			plumbing.ReferenceName(paths.V2FullRefPrefix+num),
			fullRef.Hash(),
		)
		require.NoError(t, repo.Storer.SetReference(ref))
	}

	t.Chdir(tmpDir)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	pushV2Refs(ctx, bareDir)

	// Verify all three refs exist in bare repo
	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err, "/main ref should exist in bare repo")

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err, "/full/current ref should exist in bare repo")

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullRefPrefix+"0000000000002"), true)
	require.NoError(t, err, "latest archived generation should exist in bare repo")

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullRefPrefix+"0000000000001"), true)
	assert.Error(t, err, "older archived generation should NOT be pushed")
}

// TestFetchAndMergeRef_RotationConflict verifies that when /full/current push
// fails because the remote was rotated, local data is merged into the latest
// archived generation and remote's /full/current is adopted locally.
// Not parallel: uses t.Chdir()
func TestFetchAndMergeRef_RotationConflict(t *testing.T) {
	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)

	// Create bare "remote"
	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	// Create local repo with a shared checkpoint on /full/current
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, localRepo, id.MustCheckpointID("aabbccddeeff"), "shared-session")

	// Push initial state to bare
	pushCmd := exec.CommandContext(ctx, "git", "push", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef))
	pushCmd.Dir = localDir
	require.NoError(t, pushCmd.Run())

	// Simulate remote rotation: create a second repo, fetch, add checkpoint, rotate, push
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	fetchCmd := exec.CommandContext(ctx, "git", "fetch", bareDir,
		"+"+string(fullCurrentRef)+":"+string(fullCurrentRef))
	fetchCmd.Dir = remoteDir
	require.NoError(t, fetchCmd.Run())

	remoteRepo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, remoteRepo, id.MustCheckpointID("112233445566"), "remote-session")

	// Manually rotate: archive /full/current, create fresh orphan
	remoteStore := checkpoint.NewV2GitStore(remoteRepo, "origin")
	currentRef, err := remoteRepo.Reference(fullCurrentRef, true)
	require.NoError(t, err)

	// Write generation.json and archive
	_, currentTreeHash, err := remoteStore.GetRefState(fullCurrentRef)
	require.NoError(t, err)
	gen := checkpoint.GenerationMetadata{
		OldestCheckpointAt: time.Now().UTC().Add(-time.Hour),
		NewestCheckpointAt: time.Now().UTC(),
	}
	archiveTreeHash, err := remoteStore.AddGenerationJSONToTree(currentTreeHash, gen)
	require.NoError(t, err)
	archiveCommitHash, err := checkpoint.CreateCommit(remoteRepo, archiveTreeHash,
		currentRef.Hash(), "Archive", "Test", "test@test.com")
	require.NoError(t, err)

	archiveRefName := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	require.NoError(t, remoteRepo.Storer.SetReference(
		plumbing.NewHashReference(archiveRefName, archiveCommitHash)))

	// Create fresh orphan /full/current
	emptyTree, err := checkpoint.BuildTreeFromEntries(remoteRepo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	orphanHash, err := checkpoint.CreateCommit(remoteRepo, emptyTree, plumbing.ZeroHash,
		"Start generation", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, remoteRepo.Storer.SetReference(
		plumbing.NewHashReference(fullCurrentRef, orphanHash)))

	// Push rotated state to bare (force /full/current since it's now an orphan)
	pushRotated := exec.CommandContext(ctx, "git", "push", "--force", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef),
		string(archiveRefName)+":"+string(archiveRefName))
	pushRotated.Dir = remoteDir
	out, pushErr := pushRotated.CombinedOutput()
	require.NoError(t, pushErr, "push rotated state failed: %s", out)

	// Add a local-only checkpoint
	writeV2Checkpoint(t, localRepo, id.MustCheckpointID("ffeeddccbbaa"), "local-session")

	t.Chdir(localDir)

	// fetchAndMergeRef should detect rotation and merge into the archive
	err = fetchAndMergeRef(ctx, bareDir, fullCurrentRef)
	require.NoError(t, err)

	// Verify: local /full/current should now be the fresh orphan from remote
	localRepo, err = git.PlainOpen(localDir)
	require.NoError(t, err)
	localStore := checkpoint.NewV2GitStore(localRepo, "origin")
	_, freshTreeHash, err := localStore.GetRefState(fullCurrentRef)
	require.NoError(t, err)
	freshCount, err := localStore.CountCheckpointsInTree(freshTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 0, freshCount, "local /full/current should be fresh orphan after rotation recovery")

	// Verify: archived generation should exist locally and contain the local-only checkpoint
	archiveRef, err := localRepo.Reference(archiveRefName, true)
	require.NoError(t, err)
	archiveCommit, err := localRepo.CommitObject(archiveRef.Hash())
	require.NoError(t, err)
	archiveTree, err := archiveCommit.Tree()
	require.NoError(t, err)

	// Check that the local-only checkpoint (ffeeddccbbaa) is in the archive
	_, err = archiveTree.Tree("ff/eeddccbbaa")
	require.NoError(t, err, "archived generation should contain local-only checkpoint ffeeddccbbaa")

	// Check that the shared checkpoint (aabbccddeeff) is also there
	_, err = archiveTree.Tree("aa/bbccddeeff")
	require.NoError(t, err, "archived generation should contain shared checkpoint aabbccddeeff")

	// Check that the remote checkpoint (112233445566) is also there
	_, err = archiveTree.Tree("11/2233445566")
	assert.NoError(t, err, "archived generation should contain remote checkpoint 112233445566")
}
