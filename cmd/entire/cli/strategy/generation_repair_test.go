package strategy

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairV2GenerationMetadata_RewritesGenerationJSONFromRawTranscripts(t *testing.T) {
	repo, _ := initGenerationRepairTestRepo(t)

	cpID := id.MustCheckpointID("aabbccddeeff")
	rawOldest := time.Date(2025, 12, 23, 10, 27, 44, 0, time.UTC)
	rawNewest := time.Date(2025, 12, 23, 10, 31, 37, 0, time.UTC)
	refName := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	oldCommitHash := createRepairArchivedGenerationRef(t, repo, refName, cpID, checkpoint.GenerationMetadata{
		OldestCheckpointAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		NewestCheckpointAt: time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
	}, rawOldest, rawNewest)

	result, err := RepairV2GenerationMetadata(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"0000000000001"}, result.Repaired)
	assert.Empty(t, result.Failed)

	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.NotEqual(t, oldCommitHash, ref.Hash())

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.Len(t, commit.ParentHashes, 1)
	assert.Equal(t, oldCommitHash, commit.ParentHashes[0])

	store := checkpoint.NewV2GitStore(repo, "origin")
	gen, err := store.ReadGenerationFromRef(refName)
	require.NoError(t, err)
	assert.True(t, gen.OldestCheckpointAt.Equal(rawOldest))
	assert.True(t, gen.NewestCheckpointAt.Equal(rawNewest))
	assertRepairRawTranscriptPresent(t, repo, refName, cpID)
}

func TestRepairV2GenerationMetadata_RepairsRemoteOnlyGenerationWithLease(t *testing.T) {
	repo, repoRoot := initGenerationRepairTestRepo(t)
	remoteRoot := filepath.Join(t.TempDir(), "origin.git")
	runGenerationRepairGit(t, "", "init", "--bare", remoteRoot)
	runGenerationRepairGit(t, repoRoot, "remote", "add", "origin", remoteRoot)

	cpID := id.MustCheckpointID("bbccddeeff00")
	rawOldest := time.Date(2025, 11, 1, 9, 0, 0, 0, time.UTC)
	rawNewest := time.Date(2025, 11, 1, 9, 5, 0, 0, time.UTC)
	refName := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000002")
	oldCommitHash := createRepairArchivedGenerationRef(t, repo, refName, cpID, checkpoint.GenerationMetadata{
		OldestCheckpointAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		NewestCheckpointAt: time.Date(2026, 1, 3, 1, 0, 0, 0, time.UTC),
	}, rawOldest, rawNewest)
	runGenerationRepairGit(t, repoRoot, "push", "origin", refName.String()+":"+refName.String())
	require.NoError(t, DeleteRefCLI(context.Background(), refName.String(), oldCommitHash.String()))

	result, err := RepairV2GenerationMetadata(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"0000000000002"}, result.Repaired)
	assert.Empty(t, result.Failed)

	if _, err := repo.Reference(refName, true); err == nil {
		t.Fatal("remote-only repair should not leave the archived ref as a local canonical ref")
	}
	if _, err := repo.Reference(plumbing.ReferenceName("refs/entire-clean-tmp/v2/full/0000000000002"), true); err == nil {
		t.Fatal("remote-only repair should remove the temporary fetched ref")
	}

	remoteRepo, err := git.PlainOpen(remoteRoot)
	require.NoError(t, err)
	remoteRef, err := remoteRepo.Reference(refName, true)
	require.NoError(t, err)
	require.NotEqual(t, oldCommitHash, remoteRef.Hash())

	remoteCommit, err := remoteRepo.CommitObject(remoteRef.Hash())
	require.NoError(t, err)
	require.Len(t, remoteCommit.ParentHashes, 1)
	assert.Equal(t, oldCommitHash, remoteCommit.ParentHashes[0])

	remoteStore := checkpoint.NewV2GitStore(remoteRepo, "origin")
	gen, err := remoteStore.ReadGenerationFromRef(refName)
	require.NoError(t, err)
	assert.True(t, gen.OldestCheckpointAt.Equal(rawOldest))
	assert.True(t, gen.NewestCheckpointAt.Equal(rawNewest))
}

func TestRepairV2GenerationMetadata_ExcludeRefsSkipsListedRefs(t *testing.T) {
	repo, _ := initGenerationRepairTestRepo(t)

	cpID := id.MustCheckpointID("aabb22334455")
	rawOldest := time.Date(2025, 10, 1, 8, 0, 0, 0, time.UTC)
	rawNewest := time.Date(2025, 10, 1, 8, 30, 0, 0, time.UTC)

	excludedRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000007")
	wrongGen := checkpoint.GenerationMetadata{
		OldestCheckpointAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		NewestCheckpointAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	beforeCommit := createRepairArchivedGenerationRef(t, repo, excludedRef, cpID, wrongGen, rawOldest, rawNewest)

	result, err := RepairV2GenerationMetadata(context.Background(), []plumbing.ReferenceName{excludedRef})
	require.NoError(t, err)
	assert.Empty(t, result.Repaired, "excluded ref must not be repaired")
	assert.Empty(t, result.Skipped, "excluded ref must not appear in skipped — it should never reach the per-candidate loop")

	ref, err := repo.Reference(excludedRef, true)
	require.NoError(t, err)
	assert.Equal(t, beforeCommit, ref.Hash(), "excluded ref must not advance even when its generation.json is wrong")
}

func TestRepairV2GenerationMetadata_NoCandidatesIsNoOp(t *testing.T) {
	initGenerationRepairTestRepo(t)

	result, err := RepairV2GenerationMetadata(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, result.Repaired)
	assert.Empty(t, result.Skipped)
	assert.Empty(t, result.Failed)
	assert.Empty(t, result.Warnings)
}

func TestRepairV2GenerationMetadata_AlreadyCorrectIsSkipped(t *testing.T) {
	repo, _ := initGenerationRepairTestRepo(t)

	cpID := id.MustCheckpointID("ccddeeff0011")
	rawOldest := time.Date(2025, 10, 1, 8, 0, 0, 0, time.UTC)
	rawNewest := time.Date(2025, 10, 1, 8, 30, 0, 0, time.UTC)
	refName := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000003")
	oldCommitHash := createRepairArchivedGenerationRef(t, repo, refName, cpID, checkpoint.GenerationMetadata{
		OldestCheckpointAt: rawOldest,
		NewestCheckpointAt: rawNewest,
	}, rawOldest, rawNewest)

	result, err := RepairV2GenerationMetadata(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000003"}, result.Skipped)
	assert.Empty(t, result.Repaired)
	assert.Empty(t, result.Failed)

	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	assert.Equal(t, oldCommitHash, ref.Hash(), "ref must not advance when generation.json already matches")
}

func initGenerationRepairTestRepo(t *testing.T) (*git.Repository, string) {
	t.Helper()

	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "init")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	return repo, repoRoot
}

func createRepairArchivedGenerationRef(
	t *testing.T,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	cpID id.CheckpointID,
	staleGen checkpoint.GenerationMetadata,
	rawOldest time.Time,
	rawNewest time.Time,
) plumbing.Hash {
	t.Helper()

	genJSON, err := json.Marshal(staleGen)
	require.NoError(t, err)
	genBlobHash, err := checkpoint.CreateBlobFromContent(repo, genJSON)
	require.NoError(t, err)

	transcript := fmt.Sprintf(
		"{\"type\":\"user\",\"timestamp\":%q}\n{\"type\":\"assistant\",\"timestamp\":%q}\n",
		rawOldest.Format(time.RFC3339Nano),
		rawNewest.Format(time.RFC3339Nano),
	)
	transcriptBlobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(transcript))
	require.NoError(t, err)

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		paths.GenerationFileName: {
			Name: paths.GenerationFileName,
			Mode: filemode.Regular,
			Hash: genBlobHash,
		},
		cpID.Path() + "/0/" + paths.V2RawTranscriptFileName: {
			Name: paths.V2RawTranscriptFileName,
			Mode: filemode.Regular,
			Hash: transcriptBlobHash,
		},
	})
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash,
		"archived generation\n", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
	return commitHash
}

func assertRepairRawTranscriptPresent(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, cpID id.CheckpointID) {
	t.Helper()

	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.File(cpID.Path() + "/0/" + paths.V2RawTranscriptFileName)
	require.NoError(t, err)
}

func runGenerationRepairGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %s: %v", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
}
