package strategy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

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
	emptyTree, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{})
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, emptyTree, plumbing.ZeroHash,
		"Init v2 main", "Test", "test@test.com")
	require.NoError(t, err)

	ref := plumbing.NewHashReference(plumbing.ReferenceName(paths.V2MainRefName), commitHash)
	require.NoError(t, repo.Storer.SetReference(ref))

	return tmpDir
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
		Transcript:   redact.AlreadyRedacted([]byte(`{"from":"` + sessionID + `"}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
}

func v2CheckpointCountInRef(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName) int {
	t.Helper()

	store := checkpoint.NewV2GitStore(repo, "origin")
	_, treeHash, err := store.GetRefState(refName)
	require.NoError(t, err)
	count, err := store.CountCheckpointsInTree(treeHash)
	require.NoError(t, err)
	return count
}

func writeV2ArchiveRef(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, marker string) plumbing.Hash {
	t.Helper()

	blobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(marker))
	require.NoError(t, err)
	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		paths.GenerationFileName: {
			Name: paths.GenerationFileName,
			Mode: 0o100644,
			Hash: blobHash,
		},
	})
	require.NoError(t, err)
	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash,
		"Archive generation", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
	return commitHash
}

func enableFilteredFetchesForTest(t *testing.T, dir string) {
	t.Helper()

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, paths.SettingsFileName),
		[]byte(`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`),
		0o644,
	))
}

func enableFilteredFetchServingForTest(t *testing.T, bareDir string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "-C", bareDir, "config", "uploadpack.allowFilter", "true")
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "enable uploadpack.allowFilter failed: %s", out)
}

func rotateV2CurrentForTest(t *testing.T, repo *git.Repository, archiveRefName plumbing.ReferenceName) plumbing.Hash {
	t.Helper()

	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	store := checkpoint.NewV2GitStore(repo, "origin")

	currentRef, err := repo.Reference(fullCurrentRef, true)
	require.NoError(t, err)
	_, currentTreeHash, err := store.GetRefState(fullCurrentRef)
	require.NoError(t, err)

	gen := checkpoint.GenerationMetadata{
		OldestCheckpointAt: time.Now().UTC().Add(-time.Hour),
		NewestCheckpointAt: time.Now().UTC(),
	}
	archiveTreeHash, err := store.AddGenerationJSONToTree(currentTreeHash, gen)
	require.NoError(t, err)
	archiveCommitHash, err := checkpoint.CreateCommit(ctx, repo, archiveTreeHash,
		currentRef.Hash(), "Archive", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(archiveRefName, archiveCommitHash)))

	emptyTree, err := checkpoint.BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	orphanHash, err := checkpoint.CreateCommit(ctx, repo, emptyTree, plumbing.ZeroHash,
		"Start generation", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(fullCurrentRef, orphanHash)))
	return archiveCommitHash
}

func refContainsV2Checkpoint(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, cpID id.CheckpointID) bool {
	t.Helper()

	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.Tree(cpID.Path())
	return err == nil
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

// TestPushV2Refs_SkipsUnrecordedArchiveRefs verifies that pushV2Refs pushes
// /main and /full/current, but does not push archived generations unless a
// local handoff records them as needing publication.
// Not parallel: uses t.Chdir()
func TestPushV2Refs_SkipsUnrecordedArchiveRefs(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithV2Ref(t)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Write a checkpoint (creates both /main and /full/current)
	writeV2Checkpoint(t, repo, id.MustCheckpointID("aabbccddeeff"), "test-session")

	// Create two fake archived generation refs without recording a pending
	// publication handoff.
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

	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	output := restore()

	// Verify only active refs exist in bare repo.
	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err, "/main ref should exist in bare repo")

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err, "/full/current ref should exist in bare repo")

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullRefPrefix+"0000000000002"), true)
	require.Error(t, err, "unrecorded latest archived generation should not be pushed")

	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullRefPrefix+"0000000000001"), true)
	require.Error(t, err, "unrecorded older archived generation should not be pushed")

	assert.Contains(t, output, "[entire] Syncing and pushing v2 checkpoints...")
	assert.Contains(t, output, "[entire] Pushing v2/main, v2/full/current...")
	assert.Contains(t, output, "[entire] All v2 checkpoints pushed")
	assert.NotContains(t, output, "[entire] Successfully pushed", "successful refs should only be listed on partial failure")
	assert.NotContains(t, output, "Pushing v2/main to", "per-ref progress should stay quiet")
	assert.NotContains(t, output, "Syncing v2/main with remote", "per-ref sync progress should stay quiet")
}

// TestPushV2Refs_PushesPendingArchivePublications verifies migration-created
// archived generations can be queued for the next pre-push without making every
// local archived generation part of the default push set.
//
// Not parallel: uses t.Chdir()
func TestPushV2Refs_PushesPendingArchivePublications(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithV2Ref(t)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	store := checkpoint.NewV2GitStore(repo, "origin")

	writeV2Checkpoint(t, repo, id.MustCheckpointID("aabbccddeeff"), "test-session")

	fullRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)

	var publications []checkpoint.PendingV2FullGenerationPublication
	for _, num := range []string{"0000000000001", "0000000000002"} {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + num)
		ref := plumbing.NewHashReference(refName, fullRef.Hash())
		require.NoError(t, repo.Storer.SetReference(ref))
		publications = append(publications, checkpoint.PendingV2FullGenerationPublication{
			ArchiveRefName:    refName.String(),
			ArchiveCommitHash: fullRef.Hash().String(),
		})
	}
	require.NoError(t, store.AppendPendingFullGenerationPublications(ctx, publications))

	t.Chdir(tmpDir)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	_ = restore()

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	for _, num := range []string{"0000000000001", "0000000000002"} {
		_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullRefPrefix+num), true)
		require.NoError(t, err, "pending archived generation should exist in bare repo")
	}

	remaining, err := store.ReadPendingFullGenerationPublications(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "pending archive publications should be cleared after push")
}

// TestPushV2Refs_PushesPendingArchivePublicationsWithoutActiveRefs verifies a
// pending archive publication is still honored when there is no /main or
// /full/current ref to include in the normal active-ref push set.
//
// Not parallel: uses t.Chdir()
func TestPushV2Refs_PushesPendingArchivePublicationsWithoutActiveRefs(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	configCmd := exec.CommandContext(ctx, "git", "config", "push.default", "current")
	configCmd.Dir = tmpDir
	require.NoError(t, configCmd.Run())

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	headRef, err := repo.Head()
	require.NoError(t, err)
	store := checkpoint.NewV2GitStore(repo, "origin")

	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	archiveCommitHash := writeV2ArchiveRef(t, repo, archiveRef, "pending archive")
	require.NoError(t, store.AppendPendingFullGenerationPublication(ctx, checkpoint.PendingV2FullGenerationPublication{
		ArchiveRefName:    archiveRef.String(),
		ArchiveCommitHash: archiveCommitHash.String(),
	}))

	t.Chdir(tmpDir)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	_ = restore()

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	_, err = bareRepo.Reference(archiveRef, true)
	require.NoError(t, err, "pending archived generation should be pushed even without active refs")
	_, err = bareRepo.Reference(headRef.Name(), true)
	require.Error(t, err, "empty active-ref push must not push the current branch")

	remaining, err := store.ReadPendingFullGenerationPublications(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "pending archive publication should be cleared after push")
}

// TestPushV2Refs_PendingPublicationReadErrorDoesNotReportActiveRefFailures
// verifies that a pending-publication failure is reported as such. Active refs
// have not been pushed at that point, so they must not be reported as failed
// ref pushes.
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestPushV2Refs_PendingPublicationReadErrorDoesNotReportActiveRefFailures(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithV2Ref(t)
	pendingDir := filepath.Join(tmpDir, ".git", "entire-v2-rotations")
	require.NoError(t, os.MkdirAll(pendingDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(pendingDir, "pending.json"), []byte("{"), 0o600))

	t.Chdir(tmpDir)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	output := restore()

	assert.Contains(t, output, "[entire] Warning: read pending v2 full generation publications:")
	assert.NotContains(t, output, "failed to push v2/main")
	assert.NotContains(t, output, "failed to push v2/full/current")

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.Error(t, err, "/main should not be pushed after pending-publication read failure")
	_, err = bareRepo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.Error(t, err, "/full/current should not be pushed after pending-publication read failure")
}

// TestPushV2Refs_DropsPendingResetPublicationWhenCurrentWasNotReset verifies
// that a marker recorded before a failed/crashed reset does not publish the
// would-be archive while /full/current still points at the old generation.
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestPushV2Refs_DropsPendingResetPublicationWhenCurrentWasNotReset(t *testing.T) {
	ctx := context.Background()

	tmpDir := setupRepoWithV2Ref(t)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	store := checkpoint.NewV2GitStore(repo, "origin")

	writeV2Checkpoint(t, repo, id.MustCheckpointID("aabbccddeeff"), "test-session")
	currentRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)

	emptyTree, err := checkpoint.BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	resetRoot, err := checkpoint.CreateCommit(ctx, repo, emptyTree, plumbing.ZeroHash,
		"Start generation", "Test", "test@test.com")
	require.NoError(t, err)

	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	require.NoError(t, store.AppendPendingFullGenerationPublication(ctx, checkpoint.PendingV2FullGenerationPublication{
		ArchiveRefName:           archiveRef.String(),
		ArchiveCommitHash:        currentRef.Hash().String(),
		PreviousFullCurrentHash:  currentRef.Hash().String(),
		ResetFullCurrentRootHash: resetRoot.String(),
		QueuedAt:                 time.Now().UTC(),
	}))

	t.Chdir(tmpDir)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	output := restore()

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	_, err = bareRepo.Reference(archiveRef, true)
	require.Error(t, err, "stale pending archive should not be pushed")

	remaining, err := store.ReadPendingFullGenerationPublications(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "stale pending reset publication should be cleared")
	assert.Contains(t, output, "[entire] All v2 checkpoints pushed")
	assert.NotContains(t, output, "push pending archive")
}

func TestPendingFullArchiveRefs_OnlyReturnsV2ArchiveGenerationRefs(t *testing.T) {
	t.Parallel()

	refs := pendingFullArchiveRefs([]checkpoint.PendingV2FullGenerationPublication{
		{ArchiveRefName: paths.V2FullRefPrefix + "0000000000001"},
		{ArchiveRefName: paths.V2FullRefPrefix + "0000000000001"},
		{ArchiveRefName: paths.V2FullRefPrefix + "0000000000002"},
		{ArchiveRefName: paths.V2FullCurrentRefName},
		{ArchiveRefName: paths.V2FullRefPrefix + "not-a-generation"},
		{ArchiveRefName: paths.V2FullRefPrefix + "0000000000003/extra"},
		{ArchiveRefName: paths.V2MainRefName},
		{ArchiveRefName: "refs/heads/main"},
		{ArchiveRefName: ""},
	})

	assert.Equal(t, []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001"),
		plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000002"),
	}, refs)
}

// TestPushV2Refs_LocalRotationDoesNotRehydrateArchivedCurrent verifies that
// publishing a locally rotated generation does not merge the remote old
// /full/current tree back into the fresh local /full/current.
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestPushV2Refs_LocalRotationDoesNotRehydrateArchivedCurrent(t *testing.T) {
	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")

	localDir := setupRepoWithV2Ref(t)
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	localStore := checkpoint.NewV2GitStore(localRepo, "origin")

	for i, cpID := range []id.CheckpointID{
		id.MustCheckpointID("000000000001"),
		id.MustCheckpointID("000000000002"),
		id.MustCheckpointID("000000000003"),
	} {
		writeV2Checkpoint(t, localRepo, cpID, "session-before-rotation-"+string(rune('a'+i)))
	}

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	pushCurrent := exec.CommandContext(ctx, "git", "push", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef))
	pushCurrent.Dir = localDir
	out, err = pushCurrent.CombinedOutput()
	require.NoError(t, err, "initial full/current push failed: %s", out)

	refName, rotated, err := localStore.RotateCurrentGenerationIfNeeded(ctx, 3)
	require.NoError(t, err)
	require.True(t, rotated)
	require.Equal(t, archiveRef, refName)
	assert.Equal(t, 0, v2CheckpointCountInRef(t, localRepo, fullCurrentRef))
	assert.Equal(t, 3, v2CheckpointCountInRef(t, localRepo, archiveRef))

	t.Chdir(localDir)
	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	_ = restore()

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	assert.Equal(t, 3, v2CheckpointCountInRef(t, bareRepo, archiveRef))
	assert.Equal(t, 0, v2CheckpointCountInRef(t, bareRepo, fullCurrentRef),
		"remote /full/current must stay fresh after publishing a local rotation")
}

// TestPushV2Refs_LocalRotationPublishesCurrentWorkAddedBeforePush verifies that
// publishing a pending local rotation pushes the live /full/current head, not
// just the fresh orphan root created when the rotation happened.
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestPushV2Refs_LocalRotationPublishesCurrentWorkAddedBeforePush(t *testing.T) {
	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	oldCPs := []id.CheckpointID{
		id.MustCheckpointID("000000000001"),
		id.MustCheckpointID("000000000002"),
		id.MustCheckpointID("000000000003"),
	}
	newCPs := []id.CheckpointID{
		id.MustCheckpointID("000000000004"),
		id.MustCheckpointID("000000000005"),
	}

	localDir := setupRepoWithV2Ref(t)
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	localStore := checkpoint.NewV2GitStore(localRepo, "origin")

	for i, cpID := range oldCPs {
		writeV2Checkpoint(t, localRepo, cpID, "session-before-rotation-"+string(rune('a'+i)))
	}

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	pushCurrent := exec.CommandContext(ctx, "git", "push", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef))
	pushCurrent.Dir = localDir
	out, err = pushCurrent.CombinedOutput()
	require.NoError(t, err, "initial full/current push failed: %s", out)

	refName, rotated, err := localStore.RotateCurrentGenerationIfNeeded(ctx, 3)
	require.NoError(t, err)
	require.True(t, rotated)
	require.Equal(t, archiveRef, refName)

	for i, cpID := range newCPs {
		writeV2Checkpoint(t, localRepo, cpID, "session-after-rotation-"+string(rune('a'+i)))
	}

	t.Chdir(localDir)
	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	_ = restore()

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	for _, cpID := range oldCPs {
		assert.True(t, refContainsV2Checkpoint(t, bareRepo, archiveRef, cpID),
			"archive should contain old checkpoint %s", cpID)
		assert.False(t, refContainsV2Checkpoint(t, bareRepo, fullCurrentRef, cpID),
			"current should not contain archived checkpoint %s", cpID)
	}
	for _, cpID := range newCPs {
		assert.False(t, refContainsV2Checkpoint(t, bareRepo, archiveRef, cpID),
			"archive should not contain new checkpoint %s", cpID)
		assert.True(t, refContainsV2Checkpoint(t, bareRepo, fullCurrentRef, cpID),
			"current should contain new checkpoint %s", cpID)
	}
}

// TestPushV2Refs_RepeatedLocalRotationsBeforePushPublishesAllArchives verifies
// that more than one local rotation can be queued and published in one pre-push.
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestPushV2Refs_RepeatedLocalRotationsBeforePushPublishesAllArchives(t *testing.T) {
	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	archive1Ref := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	archive2Ref := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000002")
	gen1CPs := []id.CheckpointID{
		id.MustCheckpointID("000000000001"),
		id.MustCheckpointID("000000000002"),
	}
	gen2CPs := []id.CheckpointID{
		id.MustCheckpointID("000000000003"),
		id.MustCheckpointID("000000000004"),
	}
	currentCP := id.MustCheckpointID("000000000005")

	localDir := setupRepoWithV2Ref(t)
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	localStore := checkpoint.NewV2GitStore(localRepo, "origin")

	for i, cpID := range gen1CPs {
		writeV2Checkpoint(t, localRepo, cpID, "session-gen-1-"+string(rune('a'+i)))
	}

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	pushCurrent := exec.CommandContext(ctx, "git", "push", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef))
	pushCurrent.Dir = localDir
	out, err = pushCurrent.CombinedOutput()
	require.NoError(t, err, "initial full/current push failed: %s", out)

	refName, rotated, err := localStore.RotateCurrentGenerationIfNeeded(ctx, len(gen1CPs))
	require.NoError(t, err)
	require.True(t, rotated)
	require.Equal(t, archive1Ref, refName)

	for i, cpID := range gen2CPs {
		writeV2Checkpoint(t, localRepo, cpID, "session-gen-2-"+string(rune('a'+i)))
	}
	refName, rotated, err = localStore.RotateCurrentGenerationIfNeeded(ctx, len(gen2CPs))
	require.NoError(t, err)
	require.True(t, rotated)
	require.Equal(t, archive2Ref, refName)

	writeV2Checkpoint(t, localRepo, currentCP, "session-current")

	t.Chdir(localDir)
	restore := captureStderr(t)
	pushV2Refs(ctx, bareDir)
	_ = restore()

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	for _, cpID := range gen1CPs {
		assert.True(t, refContainsV2Checkpoint(t, bareRepo, archive1Ref, cpID),
			"archive 1 should contain generation 1 checkpoint %s", cpID)
		assert.False(t, refContainsV2Checkpoint(t, bareRepo, archive2Ref, cpID),
			"archive 2 should not contain generation 1 checkpoint %s", cpID)
		assert.False(t, refContainsV2Checkpoint(t, bareRepo, fullCurrentRef, cpID),
			"current should not contain generation 1 checkpoint %s", cpID)
	}
	for _, cpID := range gen2CPs {
		assert.False(t, refContainsV2Checkpoint(t, bareRepo, archive1Ref, cpID),
			"archive 1 should not contain generation 2 checkpoint %s", cpID)
		assert.True(t, refContainsV2Checkpoint(t, bareRepo, archive2Ref, cpID),
			"archive 2 should contain generation 2 checkpoint %s", cpID)
		assert.False(t, refContainsV2Checkpoint(t, bareRepo, fullCurrentRef, cpID),
			"current should not contain generation 2 checkpoint %s", cpID)
	}
	assert.False(t, refContainsV2Checkpoint(t, bareRepo, archive1Ref, currentCP),
		"archive 1 should not contain current checkpoint")
	assert.False(t, refContainsV2Checkpoint(t, bareRepo, archive2Ref, currentCP),
		"archive 2 should not contain current checkpoint")
	assert.True(t, refContainsV2Checkpoint(t, bareRepo, fullCurrentRef, currentCP),
		"current should contain current checkpoint")
}

func TestDetectRemoteRotationArchives_IncludesSameNameDifferentHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")

	localDir := setupRepoWithV2Ref(t)
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	localHash := writeV2ArchiveRef(t, localRepo, archiveRef, "local archive")

	remoteDir := setupRepoWithV2Ref(t)
	remoteRepo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	remoteHash := writeV2ArchiveRef(t, remoteRepo, archiveRef, "remote archive")
	require.NotEqual(t, localHash, remoteHash)

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	pushArchive := exec.CommandContext(ctx, "git", "push", bareDir,
		string(archiveRef)+":"+string(archiveRef))
	pushArchive.Dir = remoteDir
	out, err = pushArchive.CombinedOutput()
	require.NoError(t, err, "archive push failed: %s", out)

	archives, err := detectRemoteRotationArchives(ctx, bareDir, localRepo)
	require.NoError(t, err)
	assert.Contains(t, archives, "0000000000001")
}

func TestPrintV2PartialPushResult(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	printV2PartialPushResult(
		&output,
		[]plumbing.ReferenceName{plumbing.ReferenceName(paths.V2MainRefName)},
		[]error{errors.New("couldn't sync v2/full/current: fetch failed")},
	)

	assert.Contains(t, output.String(), "[entire] Successfully pushed v2/main")
	assert.Contains(t, output.String(), "[entire] Warning: couldn't sync v2/full/current: fetch failed")
	assert.NotContains(t, output.String(), "[entire] All v2 checkpoints pushed")
}

func TestParsePushRefResults_MultiRefPorcelain(t *testing.T) {
	t.Parallel()

	refs := []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
		plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000002"),
	}
	output := strings.Join([]string{
		"To https://example.com/repo.git",
		"*\trefs/entire/checkpoints/v2/main:refs/entire/checkpoints/v2/main\t[new reference]",
		"!\trefs/entire/checkpoints/v2/full/current:refs/entire/checkpoints/v2/full/current\t[rejected] (non-fast-forward)",
		"=\trefs/entire/checkpoints/v2/full/0000000000002:refs/entire/checkpoints/v2/full/0000000000002\tup to date",
	}, "\n")

	results := parsePushRefResults(context.Background(), output, refs, errors.New("git push failed"))

	require.Len(t, results, 3)
	assert.Equal(t, plumbing.ReferenceName(paths.V2MainRefName), results[0].refName)
	require.NoError(t, results[0].err)
	assert.False(t, results[0].result.upToDate)
	assert.Equal(t, plumbing.ReferenceName(paths.V2FullCurrentRefName), results[1].refName)
	require.ErrorContains(t, results[1].err, "non-fast-forward")
	assert.Equal(t, plumbing.ReferenceName(paths.V2FullRefPrefix+"0000000000002"), results[2].refName)
	require.NoError(t, results[2].err)
	assert.True(t, results[2].result.upToDate)
}

func TestParsePushRefResults_MissingStatusUsesStatusMissing(t *testing.T) {
	t.Parallel()

	refs := []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
	}
	output := strings.Join([]string{
		"To https://example.com/repo.git",
		"*\trefs/entire/checkpoints/v2/main:refs/entire/checkpoints/v2/main\t[new reference]",
	}, "\n")

	results := parsePushRefResults(context.Background(), output, refs, errors.New("git push failed"))

	require.Len(t, results, 2)
	require.NoError(t, results[0].err)
	require.ErrorContains(t, results[1].err, "status missing for v2/full/current")
}

func TestParsePushRefResults_GenericRejectionDoesNotBecomeNonFastForward(t *testing.T) {
	t.Parallel()

	refs := []plumbing.ReferenceName{plumbing.ReferenceName(paths.V2MainRefName)}
	output := "!\trefs/entire/checkpoints/v2/main:refs/entire/checkpoints/v2/main\t[remote rejected] auth rejected by server"

	results := parsePushRefResults(context.Background(), output, refs, errors.New("git push failed"))

	require.Len(t, results, 1)
	require.Error(t, results[0].err)
	require.NotErrorIs(t, results[0].err, errNonFastForward)
	assert.ErrorContains(t, results[0].err, "push failed")
}

// TestPushV2Refs_UnreachableTarget_NamesFailedRef verifies that aggregated v2
// push output still identifies the ref that could not be pushed.
//
// Not parallel: uses t.Chdir() and os.Stderr redirection.
func TestPushV2Refs_UnreachableTarget_NamesFailedRef(t *testing.T) {
	tmpDir := setupRepoWithV2Ref(t)
	t.Chdir(tmpDir)

	nonExistentPath := filepath.Join(t.TempDir(), "does-not-exist")
	restore := captureStderr(t)
	pushV2Refs(context.Background(), nonExistentPath)
	output := restore()

	assert.Contains(t, output, "[entire] Syncing and pushing v2 checkpoints...")
	assert.Contains(t, output, "[entire] Pushing v2/main...")
	assert.Contains(t, output, "[entire] Warning: failed to push v2/main:")
	assert.NotContains(t, output, "[entire] Warning: couldn't sync v2/main:")
	assert.NotContains(t, output, "[entire] All v2 checkpoints pushed")
	assert.NotContains(t, output, "Pushing v2/main to", "failed aggregated pushes should avoid per-ref progress")
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
	archiveCommitHash, err := checkpoint.CreateCommit(context.Background(), remoteRepo, archiveTreeHash,
		currentRef.Hash(), "Archive", "Test", "test@test.com")
	require.NoError(t, err)

	archiveRefName := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	require.NoError(t, remoteRepo.Storer.SetReference(
		plumbing.NewHashReference(archiveRefName, archiveCommitHash)))

	// Create fresh orphan /full/current
	emptyTree, err := checkpoint.BuildTreeFromEntries(context.Background(), remoteRepo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	orphanHash, err := checkpoint.CreateCommit(context.Background(), remoteRepo, emptyTree, plumbing.ZeroHash,
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

func TestFetchAndMergeRef_RotationConflictWithFilteredFetches(t *testing.T) {
	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	archiveRefName := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	sharedCP := id.MustCheckpointID("aabbccddeeff")
	remoteCP := id.MustCheckpointID("112233445566")
	localOnlyCP := id.MustCheckpointID("ffeeddccbbaa")

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())
	enableFilteredFetchServingForTest(t, bareDir)
	bareURL := "file://" + bareDir

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	enableFilteredFetchesForTest(t, localDir)

	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, localRepo, sharedCP, "shared-session")

	pushCurrent := exec.CommandContext(ctx, "git", "push", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef))
	pushCurrent.Dir = localDir
	out, err := pushCurrent.CombinedOutput()
	require.NoError(t, err, "initial full/current push failed: %s", out)

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	fetchCurrent := exec.CommandContext(ctx, "git", "fetch", bareDir,
		"+"+string(fullCurrentRef)+":"+string(fullCurrentRef))
	fetchCurrent.Dir = remoteDir
	out, err = fetchCurrent.CombinedOutput()
	require.NoError(t, err, "fetch full/current failed: %s", out)

	remoteRepo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, remoteRepo, remoteCP, "remote-session")
	rotateV2CurrentForTest(t, remoteRepo, archiveRefName)

	pushRotated := exec.CommandContext(ctx, "git", "push", "--force", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef),
		string(archiveRefName)+":"+string(archiveRefName))
	pushRotated.Dir = remoteDir
	out, err = pushRotated.CombinedOutput()
	require.NoError(t, err, "push rotated state failed: %s", out)

	writeV2Checkpoint(t, localRepo, localOnlyCP, "local-session")

	t.Chdir(localDir)
	err = fetchAndMergeRef(ctx, bareURL, fullCurrentRef)
	require.NoError(t, err)

	localRepo, err = git.PlainOpen(localDir)
	require.NoError(t, err)
	assert.True(t, refContainsV2Checkpoint(t, localRepo, archiveRefName, sharedCP))
	assert.True(t, refContainsV2Checkpoint(t, localRepo, archiveRefName, remoteCP))
	assert.True(t, refContainsV2Checkpoint(t, localRepo, archiveRefName, localOnlyCP))
	assert.False(t, refContainsV2Checkpoint(t, localRepo, fullCurrentRef, sharedCP))
	assert.False(t, refContainsV2Checkpoint(t, localRepo, fullCurrentRef, localOnlyCP))
}

func TestFetchAndMergeRef_RemoteRotatedMultipleTimesUsesRelatedArchive(t *testing.T) {
	ctx := context.Background()
	fullCurrentRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	archive1Ref := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	archive2Ref := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000002")
	sharedCP := id.MustCheckpointID("aabbccddeeff")
	remoteGen1CP := id.MustCheckpointID("112233445566")
	remoteGen2CP := id.MustCheckpointID("223344556677")
	localOnlyCP := id.MustCheckpointID("ffeeddccbbaa")

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, initCmd.Run())

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, localRepo, sharedCP, "shared-session")

	pushCurrent := exec.CommandContext(ctx, "git", "push", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef))
	pushCurrent.Dir = localDir
	require.NoError(t, pushCurrent.Run())

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	fetchCurrent := exec.CommandContext(ctx, "git", "fetch", bareDir,
		"+"+string(fullCurrentRef)+":"+string(fullCurrentRef))
	fetchCurrent.Dir = remoteDir
	require.NoError(t, fetchCurrent.Run())

	remoteRepo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	writeV2Checkpoint(t, remoteRepo, remoteGen1CP, "remote-gen-1")
	rotateV2CurrentForTest(t, remoteRepo, archive1Ref)
	writeV2Checkpoint(t, remoteRepo, remoteGen2CP, "remote-gen-2")
	rotateV2CurrentForTest(t, remoteRepo, archive2Ref)

	pushRotated := exec.CommandContext(ctx, "git", "push", "--force", bareDir,
		string(fullCurrentRef)+":"+string(fullCurrentRef),
		string(archive1Ref)+":"+string(archive1Ref),
		string(archive2Ref)+":"+string(archive2Ref))
	pushRotated.Dir = remoteDir
	out, pushErr := pushRotated.CombinedOutput()
	require.NoError(t, pushErr, "push rotated state failed: %s", out)

	writeV2Checkpoint(t, localRepo, localOnlyCP, "local-session")

	t.Chdir(localDir)
	err = fetchAndMergeRef(ctx, bareDir, fullCurrentRef)
	require.NoError(t, err)

	localRepo, err = git.PlainOpen(localDir)
	require.NoError(t, err)
	_, err = localRepo.Reference(archiveTmpRefName("0000000000001"), true)
	require.Error(t, err, "selected archive temp ref should be removed after rotation recovery")
	_, err = localRepo.Reference(archiveTmpRefName("0000000000002"), true)
	require.Error(t, err, "unselected archive temp ref should be removed after rotation recovery")

	assert.True(t, refContainsV2Checkpoint(t, localRepo, archive1Ref, localOnlyCP),
		"local checkpoint from the first generation should be merged into archive 1")
	assert.True(t, refContainsV2Checkpoint(t, localRepo, archive1Ref, sharedCP),
		"shared first-generation checkpoint should remain in archive 1")

	bareRepo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	assert.False(t, refContainsV2Checkpoint(t, bareRepo, archive2Ref, localOnlyCP),
		"local first-generation checkpoint must not be merged into later remote archive 2")
	assert.True(t, refContainsV2Checkpoint(t, bareRepo, archive2Ref, remoteGen2CP),
		"remote generation 2 checkpoint should remain in archive 2")
}
