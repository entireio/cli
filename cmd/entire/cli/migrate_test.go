package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initMigrateTestRepo creates a repo with an initial commit.
func initMigrateTestRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "init")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	return repo
}

// writeV1Checkpoint writes a checkpoint to the v1 branch for testing.
func writeV1Checkpoint(t *testing.T, store *checkpoint.GitStore, cpID id.CheckpointID, sessionID string, transcript []byte, prompts []string) {
	t.Helper()
	err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      prompts,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
}

func newMigrateStores(repo *git.Repository) (*checkpoint.GitStore, *checkpoint.V2GitStore) {
	return checkpoint.NewGitStore(repo), checkpoint.NewV2GitStore(repo, migrateRemoteName)
}

func buildTasksTreeHashWithContent(t *testing.T, repo *git.Repository, toolUseID string, content string) plumbing.Hash {
	t.Helper()

	blobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(content))
	require.NoError(t, err)

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		toolUseID + "/checkpoint.json": {Mode: filemode.Regular, Hash: blobHash},
	})
	require.NoError(t, err)

	return treeHash
}

func addV1SessionTasksTree(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionIdx int, toolUseID string) {
	t.Helper()
	addV1SessionTasksTreeWithContent(t, repo, cpID, sessionIdx, toolUseID, `{"tool_use_id":"`+toolUseID+`"}`)
}

func addV1SessionTasksTreeWithContent(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionIdx int, toolUseID string, content string) {
	t.Helper()

	tasksTreeHash := buildTasksTreeHashWithContent(t, repo, toolUseID, content)
	tasksTree, err := repo.TreeObject(tasksTreeHash)
	require.NoError(t, err)

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	newRoot, err := checkpoint.UpdateSubtree(repo, commit.TreeHash,
		[]string{string(cpID[:2]), string(cpID[2:]), strconv.Itoa(sessionIdx), "tasks"},
		tasksTree.Entries,
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, newRoot, ref.Hash(),
		"Add test session task metadata\n",
		"Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func addV1RootTasksTreeWithContent(t *testing.T, repo *git.Repository, cpID id.CheckpointID, toolUseID string, content string) {
	t.Helper()

	tasksTreeHash := buildTasksTreeHashWithContent(t, repo, toolUseID, content)
	tasksTree, err := repo.TreeObject(tasksTreeHash)
	require.NoError(t, err)

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	newRoot, err := checkpoint.UpdateSubtree(repo, commit.TreeHash,
		[]string{string(cpID[:2]), string(cpID[2:]), "tasks"},
		tasksTree.Entries,
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, newRoot, ref.Hash(),
		"Add test root task metadata\n",
		"Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func TestMigrateCheckpointsV2_Basic(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	writeV1Checkpoint(t, v1Store, cpID, "session-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"hello\"}\n"),
		[]string{"test prompt"},
	)

	var stdout bytes.Buffer

	result, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)

	// Verify checkpoint exists in v2
	summary, err := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary, "checkpoint should exist in v2 after migration")
	assert.Equal(t, cpID, summary.CheckpointID)
}

func TestMigrateCheckpointsV2_PreservesCreatedAt(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cpID := id.MustCheckpointID("b1c2d3e4f5a6")
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-created-at",
		CreatedAt:    createdAt,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"hello\"}\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	content, err := v2Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err)
	assert.True(t, content.Metadata.CreatedAt.Equal(createdAt))
}

func TestMigrateCheckpointsV2_UnderThresholdKeepsFullGenerationInCurrent(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 5
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	checkpointIDs := []id.CheckpointID{
		id.MustCheckpointID("000000000101"),
		id.MustCheckpointID("000000000102"),
		id.MustCheckpointID("000000000103"),
	}
	transcripts := make(map[id.CheckpointID][]byte, len(checkpointIDs))
	for idx, cpID := range checkpointIDs {
		transcript := []byte(`{"type":"assistant","message":"under threshold ` + strconv.Itoa(idx) + `"}` + "\n")
		transcripts[cpID] = transcript
		writeV1Checkpoint(t, v1Store, cpID, "session-under-threshold-"+strconv.Itoa(idx), transcript, []string{"prompt"})
	}

	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 3, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)

	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Empty(t, archived, "under-threshold migration should not create archived full generations")

	_, currentTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	currentCount, err := v2Store.CountCheckpointsInTree(currentTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 3, currentCount)

	currentTree, err := repo.TreeObject(currentTreeHash)
	require.NoError(t, err)
	_, err = currentTree.File(paths.GenerationFileName)
	require.Error(t, err, "/full/current should not contain generation metadata")

	for _, cpID := range checkpointIDs {
		content, readErr := v2Store.ReadSessionContent(ctx, cpID, 0)
		require.NoError(t, readErr)
		assert.Equal(t, transcripts[cpID], content.Transcript)
	}
}

func TestMigrateCheckpointsV2_RotatesCurrentWhenFinalPartialReachesThreshold(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 2
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	existingID := id.MustCheckpointID("000000000201")
	existingTranscript := []byte(`{"type":"assistant","message":"existing current"}` + "\n")
	err := v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: existingID,
		SessionID:    "session-existing-current",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(existingTranscript),
		Prompts:      []string{"existing prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	migratedID := id.MustCheckpointID("000000000202")
	migratedTranscript := []byte(`{"type":"assistant","message":"migrated current"}` + "\n")
	writeV1Checkpoint(t, v1Store, migratedID, "session-migrated-current", migratedTranscript, []string{"migrated prompt"})

	var stdout bytes.Buffer
	result, writtenRefs, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)
	require.Equal(t, []plumbing.ReferenceName{plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")}, writtenRefs)

	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Equal(t, []string{"0000000000001"}, archived)

	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + archived[0])
	_, archiveTreeHash, err := v2Store.GetRefState(archiveRef)
	require.NoError(t, err)
	archiveCount, err := v2Store.CountCheckpointsInTree(archiveTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 2, archiveCount)

	_, currentTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	currentCount, err := v2Store.CountCheckpointsInTree(currentTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 0, currentCount, "threshold rotation should reset /full/current")

	existingContent, err := v2Store.ReadSessionContent(ctx, existingID, 0)
	require.NoError(t, err)
	assert.Equal(t, existingTranscript, existingContent.Transcript)

	migratedContent, err := v2Store.ReadSessionContent(ctx, migratedID, 0)
	require.NoError(t, err)
	assert.Equal(t, migratedTranscript, migratedContent.Transcript)
}

func TestMigrateCheckpointsV2_PacksFullGenerationsOldestFirst(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 2
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	checkpointIDs := []id.CheckpointID{
		id.MustCheckpointID("000000000001"),
		id.MustCheckpointID("000000000002"),
		id.MustCheckpointID("000000000003"),
		id.MustCheckpointID("000000000004"),
		id.MustCheckpointID("000000000005"),
	}
	createdAt := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
	}

	// Write in non-chronological order to prove migration repacks by checkpoint time,
	// not v1 tree traversal or v1 ListCommitted's newest-first order.
	for _, idx := range []int{3, 1, 4, 0, 2} {
		err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
			CheckpointID: checkpointIDs[idx],
			SessionID:    "session-pack-" + strconv.Itoa(idx),
			CreatedAt:    createdAt[idx],
			Strategy:     "manual-commit",
			Transcript: redact.AlreadyRedacted([]byte(
				`{"type":"assistant","message":"checkpoint ` + strconv.Itoa(idx) + `"}` + "\n",
			)),
			Prompts:     []string{"prompt " + strconv.Itoa(idx)},
			AuthorName:  "Test",
			AuthorEmail: "test@test.com",
		})
		require.NoError(t, err)
	}

	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 5, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)

	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Equal(t, []string{"0000000000001", "0000000000002"}, archived)

	expectedBatches := [][]int{
		{0, 1},
		{2, 3},
	}
	for genIdx, batch := range expectedBatches {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + archived[genIdx])
		gen, genErr := v2Store.ReadGenerationFromRef(refName)
		require.NoError(t, genErr)
		assert.True(t, gen.OldestCheckpointAt.Equal(createdAt[batch[0]]), "generation %s oldest", archived[genIdx])
		assert.True(t, gen.NewestCheckpointAt.Equal(createdAt[batch[len(batch)-1]]), "generation %s newest", archived[genIdx])

		_, treeHash, refErr := v2Store.GetRefState(refName)
		require.NoError(t, refErr)
		count, countErr := v2Store.CountCheckpointsInTree(treeHash)
		require.NoError(t, countErr)
		assert.Equal(t, len(batch), count)

		tree, treeErr := repo.TreeObject(treeHash)
		require.NoError(t, treeErr)
		for _, idx := range batch {
			_, treeErr = tree.Tree(checkpointIDs[idx].Path())
			require.NoError(t, treeErr, "generation %s should contain checkpoint %s", archived[genIdx], checkpointIDs[idx])
		}
	}

	_, currentTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	currentCount, err := v2Store.CountCheckpointsInTree(currentTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 1, currentCount, "fresh migration should leave final partial batch in /full/current")

	currentTree, err := repo.TreeObject(currentTreeHash)
	require.NoError(t, err)
	_, err = currentTree.Tree(checkpointIDs[4].Path())
	require.NoError(t, err, "/full/current should contain final partial checkpoint")
}

func TestUpdateV2FullCurrentRefRejectsConcurrentChange(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	ctx := context.Background()

	treeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	baseCommit, err := checkpoint.CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		"base current\n", "Test", "test@test.com")
	require.NoError(t, err)
	concurrentCommit, err := checkpoint.CreateCommit(ctx, repo, treeHash, baseCommit,
		"concurrent current\n", "Test", "test@test.com")
	require.NoError(t, err)
	candidateCommit, err := checkpoint.CreateCommit(ctx, repo, treeHash, baseCommit,
		"candidate current\n", "Test", "test@test.com")
	require.NoError(t, err)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, baseCommit)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, concurrentCommit)))

	err = updateV2FullCurrentRef(ctx, repo, baseCommit, candidateCommit)
	require.ErrorIs(t, err, storage.ErrReferenceHasChanged)

	currentRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	assert.Equal(t, concurrentCommit, currentRef.Hash())
}

func TestUpdateV2FullCurrentRefRejectsConcurrentCreation(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	ctx := context.Background()

	treeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	concurrentCommit, err := checkpoint.CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		"concurrent current\n", "Test", "test@test.com")
	require.NoError(t, err)
	candidateCommit, err := checkpoint.CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		"candidate current\n", "Test", "test@test.com")
	require.NoError(t, err)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, concurrentCommit)))

	err = updateV2FullCurrentRef(ctx, repo, plumbing.ZeroHash, candidateCommit)
	require.ErrorIs(t, err, storage.ErrReferenceHasChanged)

	currentRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	assert.Equal(t, concurrentCommit, currentRef.Hash())
}

func TestMigrateCheckpointsV2_PacksFullGenerationMetadataFromRawTranscriptTimestamps(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 1
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("101112131415")
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rawOldest := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	rawNewest := time.Date(2026, 3, 10, 9, 5, 0, 0, time.UTC)
	transcript := []byte(
		`{"type":"user","timestamp":"` + rawOldest.Format(time.RFC3339Nano) + `"}` + "\n" +
			`{"type":"assistant","timestamp":"` + rawNewest.Format(time.RFC3339Nano) + `"}` + "\n",
	)

	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-raw-timestamps",
		CreatedAt:    createdAt,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      []string{"raw timestamp prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Equal(t, []string{"0000000000001"}, archived)

	gen, err := v2Store.ReadGenerationFromRef(plumbing.ReferenceName(paths.V2FullRefPrefix + archived[0]))
	require.NoError(t, err)
	assert.True(t, gen.OldestCheckpointAt.Equal(rawOldest))
	assert.True(t, gen.NewestCheckpointAt.Equal(rawNewest))
	assert.False(t, gen.OldestCheckpointAt.Equal(createdAt), "raw transcript timestamps should take precedence over checkpoint metadata")
}

// A migration interrupted between the /main write and the packer flush must
// resume on a non-force rerun and finish packing the missing /full artifacts.
func TestMigrateCheckpointsV2_RerunResumesInterruptedMigration(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("000000000011")
	writeV1Checkpoint(t, v1Store, cpID, "session-interrupt",
		[]byte(`{"type":"assistant","message":"hi"}`+"\n"),
		[]string{"prompt"},
	)

	// Simulate an interrupted prior migration: /main is written but the raw
	// transcript never reached /full/* (we drop the fullCheckpoint that
	// would otherwise have been fed to the packer).
	v1List, err := v1Store.ListCommitted(ctx)
	require.NoError(t, err)
	require.Len(t, v1List, 1)
	fullCheckpoint, _, migrateErr := migrateOneCheckpoint(ctx, repo, v1Store, v2Store, v1List[0], false, nil)
	require.NoError(t, migrateErr)
	require.NotNil(t, fullCheckpoint)

	hasFullBefore, err := v2Store.HasFullSessionArtifacts(cpID, 0)
	require.NoError(t, err)
	require.False(t, hasFullBefore, "precondition: full artifacts should be missing")

	// Rerun without --force: must pick up the interrupted checkpoint and
	// finish packing it. Counted as migrated, not skipped.
	var rerun bytes.Buffer
	result, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &rerun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)

	hasFullAfter, err := v2Store.HasFullSessionArtifacts(cpID, 0)
	require.NoError(t, err)
	assert.True(t, hasFullAfter, "rerun should pack the missing raw transcript")

	// A second rerun once everything is packed must skip (no further work).
	var second bytes.Buffer
	result2, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &second, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
}

func TestMigrateCheckpointsV2_Idempotent(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("c3d4e5f6a1b2")
	writeV1Checkpoint(t, v1Store, cpID, "session-idem",
		[]byte("{\"type\":\"assistant\",\"message\":\"idempotent test\"}\n"),
		[]string{"idem prompt"},
	)

	var stdout bytes.Buffer

	// First run: should migrate
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)
	assert.Equal(t, 0, result1.skipped)

	// Second run: should skip (no agent type means backfill also can't produce compact transcript)
	stdout.Reset()
	result2, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
}

func TestMigrateCheckpointsV2_ForceOverwritesExisting(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("f0f1f2f3f4f5")
	writeV1Checkpoint(t, v1Store, cpID, "session-force",
		[]byte("{\"type\":\"assistant\",\"message\":\"original\"}\n"),
		[]string{"original prompt"},
	)

	var stdout bytes.Buffer

	// First run: normal migration
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	// Second run without force: should skip
	stdout.Reset()
	result2, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)

	// Third run with force: should re-migrate
	stdout.Reset()
	result3, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result3.migrated)
	assert.Equal(t, 0, result3.skipped)
	assert.Equal(t, "✓ Packing migrated raw transcripts\n", stdout.String())

	// Verify checkpoint still readable in v2
	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	assert.Equal(t, cpID, summary.CheckpointID)

	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Empty(t, archived, "under-threshold force migration should not create archived raw transcripts")

	_, currentTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	currentCount, err := v2Store.CountCheckpointsInTree(currentTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 1, currentCount, "under-threshold force migration should leave raw transcripts in /full/current")
}

func TestMigrateCheckpointsV2_ForceMultipleCheckpoints(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID1 := id.MustCheckpointID("a0a1a2a3a4a5")
	cpID2 := id.MustCheckpointID("b0b1b2b3b4b5")
	writeV1Checkpoint(t, v1Store, cpID1, "session-force-1",
		[]byte("{\"type\":\"assistant\",\"message\":\"first\"}\n"),
		[]string{"prompt 1"},
	)
	writeV1Checkpoint(t, v1Store, cpID2, "session-force-2",
		[]byte("{\"type\":\"assistant\",\"message\":\"second\"}\n"),
		[]string{"prompt 2"},
	)

	// First run: migrates both
	var discard bytes.Buffer
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &discard, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result1.migrated)

	// Force re-migrate: should re-migrate both (0 skipped)
	var stdout bytes.Buffer
	result2, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, err)
	assert.Equal(t, 2, result2.migrated)
	assert.Equal(t, 0, result2.skipped)
}

func TestPruneV2CheckpointForForce_RecomputesPartialArchivedGeneration(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 2
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID1 := id.MustCheckpointID("101010101010")
	cpID2 := id.MustCheckpointID("202020202020")
	cp1CreatedAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	cp2CreatedAt := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	for _, cp := range []struct {
		id        id.CheckpointID
		sessionID string
		createdAt time.Time
	}{
		{cpID1, "session-force-prune-1", cp1CreatedAt},
		{cpID2, "session-force-prune-2", cp2CreatedAt},
	} {
		err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
			CheckpointID: cp.id,
			SessionID:    cp.sessionID,
			CreatedAt:    cp.createdAt,
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"force prune\"}\n")),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}

	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.migrated)

	require.NoError(t, pruneV2CheckpointForForce(ctx, repo, v2Store, cpID1))

	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Equal(t, []string{"0000000000001"}, archived)

	refName := plumbing.ReferenceName(paths.V2FullRefPrefix + archived[0])
	_, treeHash, err := v2Store.GetRefState(refName)
	require.NoError(t, err)
	count, err := v2Store.CountCheckpointsInTree(treeHash)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	rootTree, err := repo.TreeObject(treeHash)
	require.NoError(t, err)
	_, err = rootTree.Tree(cpID1.Path())
	require.Error(t, err, "force prune should remove the target checkpoint from archived generations")
	_, err = rootTree.Tree(cpID2.Path())
	require.NoError(t, err, "force prune should preserve other checkpoints in the archived generation")

	gen, err := v2Store.ReadGenerationFromRef(refName)
	require.NoError(t, err)
	assert.True(t, gen.OldestCheckpointAt.Equal(cp2CreatedAt))
	assert.True(t, gen.NewestCheckpointAt.Equal(cp2CreatedAt))
}

func TestMigrateCmd_ForceFlag(t *testing.T) {
	t.Parallel()
	cmd := newMigrateCmd()

	// Verify --force flag exists
	flag := cmd.Flags().Lookup("force")
	require.NotNil(t, flag, "--force flag should be registered")
	assert.Equal(t, "false", flag.DefValue)
}

func TestMigrateCmd_RepairsArchivedGenerationMetadata(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 1
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	t.Chdir(wt.Filesystem.Root())
	paths.ClearWorktreeRootCache()

	// A pre-existing archived generation with wrong generation.json
	// timestamps. The repair pass should rewrite this when triggered.
	malformedCpID := id.MustCheckpointID("123456789abc")
	rawOldest := time.Date(2025, 12, 20, 8, 0, 0, 0, time.UTC)
	rawNewest := time.Date(2025, 12, 20, 8, 5, 0, 0, time.UTC)
	createArchivedGenerationRefWithRawTranscript(t, repo, "0000000000007", malformedCpID,
		time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 7, 1, 0, 0, 0, time.UTC),
		rawOldest, rawNewest)

	// A real v1 checkpoint that the migration will archive — without this,
	// migration is a no-op and (correctly) skips the repair pass to avoid
	// gigabytes of unconditional transcript-blob reads on every rerun.
	v1Store, _ := newMigrateStores(repo)
	v1cpID := id.MustCheckpointID("aabbccdd0011")
	writeV1Checkpoint(t, v1Store, v1cpID, "session-trigger-repair",
		[]byte(`{"type":"assistant","message":"trigger repair"}`+"\n"),
		[]string{"prompt"},
	)

	cmd := newMigrateCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--checkpoints", "v2"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, stdout.String(), "Archived generation metadata repair: 1 repaired")
	assert.Equal(t, "✓ Packing migrated raw transcripts\n✓ Repairing archived generation metadata\n", stderr.String())

	v2Store := checkpoint.NewV2GitStore(repo, migrateRemoteName)
	gen, genErr := v2Store.ReadGenerationFromRef(plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000007"))
	require.NoError(t, genErr)
	assert.True(t, gen.OldestCheckpointAt.Equal(rawOldest))
	assert.True(t, gen.NewestCheckpointAt.Equal(rawNewest))
}

// On a no-op rerun the repair pass must be skipped — running it would do a
// transcript-blob walk per archived /full/<n>, minutes-to-hours on big repos.
func TestMigrateCmd_NoOpRerunSkipsGenerationMetadataRepair(t *testing.T) {
	repo := initMigrateTestRepo(t)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	t.Chdir(wt.Filesystem.Root())
	paths.ClearWorktreeRootCache()

	v1Store, v2Store := newMigrateStores(repo)
	cpID := id.MustCheckpointID("ffeeddccbbaa")
	writeV1Checkpoint(t, v1Store, cpID, "session-noop-rerun",
		[]byte(`{"type":"assistant","message":"first"}`+"\n"),
		[]string{"prompt"},
	)

	// First run: migrates the checkpoint. Repair output is allowed but not asserted.
	first := newMigrateCmd()
	var firstOut, firstErr bytes.Buffer
	first.SetOut(&firstOut)
	first.SetErr(&firstErr)
	first.SetArgs([]string{"--checkpoints", "v2"})
	require.NoError(t, first.Execute())

	summary, err := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary, "first run should leave the checkpoint in /main")

	// Second run: nothing to do. Repair pass must not run.
	second := newMigrateCmd()
	var secondOut, secondErr bytes.Buffer
	second.SetOut(&secondOut)
	second.SetErr(&secondErr)
	second.SetArgs([]string{"--checkpoints", "v2"})
	require.NoError(t, second.Execute())

	assert.Contains(t, secondOut.String(), "Migration complete: 0 migrated, 1 skipped")
	assert.NotContains(t, secondOut.String(), "Archived generation metadata repair",
		"no-op rerun must not invoke RepairV2GenerationMetadata")
}

func TestMigrateCheckpointsV2_MultiSession(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("d4e5f6a1b2c3")

	// Write first session
	writeV1Checkpoint(t, v1Store, cpID, "session-multi-1",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 1\"}\n"),
		[]string{"prompt 1"},
	)

	// Write second session to same checkpoint
	writeV1Checkpoint(t, v1Store, cpID, "session-multi-2",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 2\"}\n"),
		[]string{"prompt 2"},
	)

	var stdout bytes.Buffer

	result, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	// Verify both sessions are in v2
	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	assert.GreaterOrEqual(t, len(summary.Sessions), 2, "should have at least 2 sessions")
}

func TestMigrateCheckpointsV2_SkipsV1SessionWithoutTranscript(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("445566778899")

	writeV1Checkpoint(t, v1Store, cpID, "session-real",
		[]byte("{\"type\":\"assistant\",\"message\":\"real session\"}\n"),
		[]string{"real prompt"},
	)

	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-without-transcript",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)
	assert.Equal(t, 1, result.missingSessions)

	output := stdout.String()
	assert.NotContains(t, output, "warning: skipping v1 session 1")
	assert.NotContains(t, output, "skipped 1 session(s) with missing transcript/session content")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, "/"+cpID.Path()+"/0/metadata.json", summary.Sessions[0].Metadata)
}

func TestMigrateCheckpointsV2_SkipsV1SessionWithMissingDirectory(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("4455667788aa")
	writeV1Checkpoint(t, v1Store, cpID, "session-real",
		[]byte("{\"type\":\"assistant\",\"message\":\"real session\"}\n"),
		[]string{"real prompt"},
	)
	appendMissingV1SessionReference(t, repo, v1Store, cpID)

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)
	assert.Equal(t, 1, result.missingSessions)

	output := stdout.String()
	assert.NotContains(t, output, "warning: skipping v1 session 1")
	assert.NotContains(t, output, "skipped 1 session(s) with missing transcript/session content")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, "/"+cpID.Path()+"/0/metadata.json", summary.Sessions[0].Metadata)
}

func TestMigrateCheckpointsV2_TaskMetadataUsesMigratedSessionIndexAfterSkip(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("66778899aabb")

	writeV1Checkpoint(t, v1Store, cpID, "session-real",
		[]byte("{\"type\":\"assistant\",\"message\":\"real session\"}\n"),
		[]string{"real prompt"},
	)

	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-without-transcript",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	err = v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-task",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"task session\"}\n")),
		Prompts:      []string{"task prompt"},
		IsTask:       true,
		ToolUseID:    "toolu_root_shifted",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
	addV1SessionTasksTree(t, repo, cpID, 2, "toolu_session_shifted")

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 2)
	assert.Equal(t, "/"+cpID.Path()+"/1/metadata.json", summary.Sessions[1].Metadata)

	rootTree := v2FullTreeForCheckpoint(t, repo, v2Store, cpID)

	_, err = rootTree.File(cpID.Path() + "/1/tasks/toolu_root_shifted/checkpoint.json")
	require.NoError(t, err, "root task metadata should follow the shifted v2 session index")
	_, err = rootTree.File(cpID.Path() + "/1/tasks/toolu_session_shifted/checkpoint.json")
	require.NoError(t, err, "session task metadata should follow the shifted v2 session index")
	_, err = rootTree.File(cpID.Path() + "/2/tasks/toolu_root_shifted/checkpoint.json")
	require.Error(t, err, "task metadata must not be written under a non-existent v2 session")
}

func TestMigrateCheckpointsV2_TaskMetadataKeepsFirstConflictingTaskTree(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("8899aabbccdd")
	toolUseID := "toolu_conflict"
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-conflict",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"conflict\"}\n")),
		Prompts:      []string{"conflict prompt"},
		IsTask:       true,
		ToolUseID:    toolUseID,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
	addV1RootTasksTreeWithContent(t, repo, cpID, toolUseID, `{"source":"root"}`)
	addV1SessionTasksTreeWithContent(t, repo, cpID, 0, toolUseID, `{"source":"session"}`)

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	rootTree := v2FullTreeForCheckpoint(t, repo, v2Store, cpID)
	file, err := rootTree.File(cpID.Path() + "/0/tasks/" + toolUseID + "/checkpoint.json")
	require.NoError(t, err)
	content, err := file.Contents()
	require.NoError(t, err)
	assert.JSONEq(t, `{"source":"root"}`, content)
}

// Resuming an interrupted migration must not relocate the root-level v1 task
// tree onto an older session being repacked — the tree belongs at the latest
// v2 session, and attaching it elsewhere would leave duplicates once the new
// /full/<n> archive lands alongside the existing one.
func TestMigrateCheckpointsV2_RerunPartialPackDoesNotMoveRootTaskMetadataToMissingSession(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("99aabbccddee")
	rootToolUseID := "toolu_root_partial"

	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-old",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"old\"}\n")),
		Prompts:      []string{"old prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-latest",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"latest\"}\n")),
		Prompts:      []string{"latest prompt"},
		IsTask:       true,
		ToolUseID:    rootToolUseID,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	addV1RootTasksTreeWithContent(t, repo, cpID, rootToolUseID, `{"source":"root"}`)

	var initialRun bytes.Buffer
	result1, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	require.Equal(t, 1, result1.migrated)
	require.True(t, v2FullFileExistsForCheckpoint(t, repo, v2Store, cpID, "1/tasks/"+rootToolUseID+"/checkpoint.json"),
		"precondition: initial migration must attach root tasks to the latest v2 session")

	// Drop session 0's raw transcript files only — simulating a migration
	// that wrote /main but failed to fully populate /full/* for the older
	// session. Session 1's transcript and tasks remain on /full/*.
	removeV2SessionTranscriptFiles(t, repo, v2Store, cpID, 0)

	var rerun bytes.Buffer
	result2, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &rerun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result2.migrated, "resume must repack the missing session")

	assert.False(t, v2FullFileExistsForCheckpoint(t, repo, v2Store, cpID, "0/tasks/"+rootToolUseID+"/checkpoint.json"),
		"resume must not attach root task metadata to the older missing session")
	assert.True(t, v2FullFileExistsForCheckpoint(t, repo, v2Store, cpID, "1/tasks/"+rootToolUseID+"/checkpoint.json"),
		"root task metadata should stay attached to the latest v2 session")
}

func TestMigrateCheckpointsV2_SkipsCheckpointWhenAllV1SessionsMissingTranscript(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("5566778899bb")
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "metadata-only-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 0, result.migrated)
	assert.Equal(t, 1, result.skipped)
	assert.Equal(t, 0, result.failed)
	assert.Equal(t, 1, result.missingSessions)

	output := stdout.String()
	assert.NotContains(t, output, "warning: skipping v1 session 0")
	assert.NotContains(t, output, "skipped (no migratable v1 sessions")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	assert.Nil(t, summary)
}

func TestMigrateCheckpointsV2_ForcePrunesSkippedV2Sessions(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("778899aabbcc")
	writeV1Checkpoint(t, v1Store, cpID, "session-keep",
		[]byte("{\"type\":\"assistant\",\"message\":\"keep\"}\n"),
		[]string{"keep prompt"},
	)
	writeV1Checkpoint(t, v1Store, cpID, "session-stale",
		[]byte("{\"type\":\"assistant\",\"message\":\"stale\"}\n"),
		[]string{"stale prompt"},
	)

	var initialRun bytes.Buffer
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	initialSummary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, initialSummary)
	require.Len(t, initialSummary.Sessions, 2)

	err = v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-stale",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only stale prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result2, _, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, rerunErr)
	assert.Equal(t, 1, result2.migrated)
	assert.Equal(t, 0, result2.skipped)
	assert.Equal(t, 1, result2.missingSessions)
	assert.NotContains(t, stdout.String(), "warning: skipping v1 session 1")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, "/"+cpID.Path()+"/0/metadata.json", summary.Sessions[0].Metadata)

	_, rootTreeHash, refErr := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, refErr)
	rootTree, treeErr := repo.TreeObject(rootTreeHash)
	require.NoError(t, treeErr)
	_, err = rootTree.File(cpID.Path() + "/1/" + paths.V2RawTranscriptHashFileName)
	require.Error(t, err, "force migration should remove stale full transcript data for skipped sessions")
}

func TestMigrateCheckpointsV2_ForcePruneRemovesEmptyShardWhenAllSessionsSkipped(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("8899aabbccdd")
	writeV1Checkpoint(t, v1Store, cpID, "session-stale-only",
		[]byte("{\"type\":\"assistant\",\"message\":\"stale only\"}\n"),
		[]string{"stale prompt"},
	)

	var initialRun bytes.Buffer
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	err = v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-stale-only",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only stale prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result2, _, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, rerunErr)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
	assert.Equal(t, 1, result2.missingSessions)
	assert.NotContains(t, stdout.String(), "no migratable v1 sessions")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	assert.Nil(t, summary)

	assertNoV2ShardPrefix(t, repo, v2Store, plumbing.ReferenceName(paths.V2MainRefName), cpID)
	assertNoV2ShardPrefix(t, repo, v2Store, plumbing.ReferenceName(paths.V2FullCurrentRefName), cpID)
}

func assertNoV2ShardPrefix(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, cpID id.CheckpointID) {
	t.Helper()

	_, rootTreeHash, err := v2Store.GetRefState(refName)
	require.NoError(t, err)

	rootTree, err := repo.TreeObject(rootTreeHash)
	require.NoError(t, err)

	_, err = rootTree.Tree(string(cpID[:2]))
	require.Error(t, err, "force prune should remove an empty shard prefix from %s", refName)
}

func appendMissingV1SessionReference(t *testing.T, repo *git.Repository, v1Store *checkpoint.GitStore, cpID id.CheckpointID) {
	t.Helper()

	ctx := context.Background()
	summary, err := v1Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)

	missingIndex := len(summary.Sessions)
	missingBase := "/" + cpID.Path() + "/" + strconv.Itoa(missingIndex) + "/"
	summary.Sessions = append(summary.Sessions, checkpoint.SessionFilePaths{
		Metadata:    missingBase + paths.MetadataFileName,
		Transcript:  missingBase + paths.TranscriptFileName,
		ContentHash: missingBase + paths.ContentHashFileName,
		Prompt:      missingBase + paths.PromptFileName,
	})

	metadataJSON, err := json.MarshalIndent(summary, "", "  ")
	require.NoError(t, err)
	metadataJSON = append(metadataJSON, '\n')

	metadataHash, err := checkpoint.CreateBlobFromContent(repo, metadataJSON)
	require.NoError(t, err)

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	newTreeHash, err := checkpoint.UpdateSubtree(
		repo,
		commit.TreeHash,
		[]string{string(cpID[:2]), string(cpID[2:])},
		[]object.TreeEntry{{
			Name: paths.MetadataFileName,
			Mode: filemode.Regular,
			Hash: metadataHash,
		}},
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	require.NoError(t, err)

	newCommitHash, err := checkpoint.CreateCommit(ctx, repo, newTreeHash, ref.Hash(), "test: stale v1 session reference\n", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, newCommitHash)))
}

func TestMigrateCheckpointsV2_NoV1Branch(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	var stdout bytes.Buffer

	// No v1 data written — ListCommitted returns empty
	result, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.migrated)
	assert.Empty(t, stdout.String())
}

func TestMigrateCmd_InvalidFlag(t *testing.T) {
	t.Parallel()
	cmd := newMigrateCmd()
	cmd.SetArgs([]string{"--checkpoints", "v3"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported checkpoints version")
}

func TestMigrateCheckpointsV2_CompactionSkipped(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("e5f6a1b2c3d4")
	// Write checkpoint with no agent type — compaction will be skipped
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-noagent",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"no agent\"}\n")),
		Prompts:      []string{"compact fail prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer

	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 1, result.compactTranscriptSkipped)
	assert.Equal(t, "✓ Packing migrated raw transcripts\n", stdout.String())
}

func TestMigrateCheckpointsV2_TaskCheckpoint(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("b2c3d4e5f6a1")
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-task-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"task work\"}\n")),
		Prompts:      []string{"task prompt"},
		IsTask:       true,
		ToolUseID:    "toolu_01ABC",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer

	result, _, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	// Verify task checkpoint exists in v2
	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)

	// Verify task metadata tree was copied into the migrated v2 /full/* generation.
	rootTree := v2FullTreeForCheckpoint(t, repo, v2Store, cpID)
	_, taskFileErr := rootTree.File(cpID.Path() + "/0/tasks/toolu_01ABC/checkpoint.json")
	require.NoError(t, taskFileErr, "expected migrated task checkpoint metadata in /full/*")
}

// TestMigrateCheckpointsV2_RerunPicksUpNewV1Checkpoints verifies that v1
// checkpoints added after a prior migration are migrated on rerun, while
// already-migrated checkpoints stay skipped.
func TestMigrateCheckpointsV2_RerunPicksUpNewV1Checkpoints(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpExisting := id.MustCheckpointID("aaa111222333")
	writeV1Checkpoint(t, v1Store, cpExisting, "session-existing",
		[]byte(`{"type":"assistant","message":"existing"}`+"\n"),
		[]string{"existing prompt"},
	)

	var firstRun bytes.Buffer
	r1, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &firstRun, false)
	require.NoError(t, err)
	require.Equal(t, 1, r1.migrated)

	// Add a new v1 checkpoint after the initial migration completed.
	cpNew := id.MustCheckpointID("bbb444555666")
	writeV1Checkpoint(t, v1Store, cpNew, "session-new",
		[]byte(`{"type":"assistant","message":"new"}`+"\n"),
		[]string{"new prompt"},
	)

	// Rerun: existing must be skipped, new one must be migrated.
	var rerun bytes.Buffer
	r2, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &rerun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r2.migrated, "new v1 checkpoint should be migrated on rerun")
	assert.Equal(t, 1, r2.skipped, "already-migrated v1 checkpoint should be skipped")

	for _, cp := range []id.CheckpointID{cpExisting, cpNew} {
		hasFull, err := v2Store.HasFullSessionArtifacts(cp, 0)
		require.NoError(t, err)
		assert.True(t, hasFull, "checkpoint %s should have full artifacts after rerun", cp)
	}
}

func TestMigrateCheckpointsV2_AllSkippedOnRerun(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID1 := id.MustCheckpointID("f6a1b2c3d4e5")
	cpID2 := id.MustCheckpointID("a1b2c3d4e5f7")

	writeV1Checkpoint(t, v1Store, cpID1, "session-p1",
		[]byte("{\"type\":\"assistant\",\"message\":\"first\"}\n"),
		[]string{"prompt 1"},
	)
	writeV1Checkpoint(t, v1Store, cpID2, "session-p2",
		[]byte("{\"type\":\"assistant\",\"message\":\"second\"}\n"),
		[]string{"prompt 2"},
	)

	// First run: migrates both
	var discard bytes.Buffer
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &discard, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result1.migrated)

	// Second run: skips both
	var stdout bytes.Buffer
	result2, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 2, result2.skipped)
}

func TestMigrateCheckpointsV2_BackfillsCompactTranscriptWhenFullArtifactsExist(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("aabb11223344")
	transcript := []byte(
		"{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n" +
			"{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\n",
	)

	err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-backfill",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      []string{"hello"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	err = v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-backfill",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      []string{"hello"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	summary, err := v2Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Empty(t, summary.Sessions[0].Transcript, "precondition: compact transcript should be missing on /main")
	hasFull, err := v2Store.HasFullSessionArtifacts(cpID, 0)
	require.NoError(t, err)
	require.True(t, hasFull, "precondition: raw /full artifacts should already exist")

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated, "compact backfill should count as migration work")
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)

	summary, err = v2Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.NotEmpty(t, summary.Sessions[0].Transcript, "compact transcript should be backfilled on /main")
}

func TestMigrateCheckpointsV2_UsesComputedCompactTranscriptStart(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("5566778899aa")
	transcript := []byte(
		"{\"type\":\"human\",\"message\":{\"content\":\"prompt 1\"}}\n" +
			"{\"type\":\"assistant\",\"message\":{\"content\":\"reply 1\"}}\n" +
			"{\"type\":\"human\",\"message\":{\"content\":\"prompt 2\"}}\n" +
			"{\"type\":\"assistant\",\"message\":{\"content\":\"reply 2\"}}\n",
	)
	err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-compact-start-migrate",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(transcript),
		Prompts:                   []string{"prompt 2"},
		Agent:                     agent.AgentTypeClaudeCode,
		CheckpointTranscriptStart: 2, // full transcript line domain
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	})
	require.NoError(t, err)

	v1Content, err := v1Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	fullCompacted := tryCompactTranscript(ctx, v1Content.Transcript, v1Content.Metadata)
	require.NotNil(t, fullCompacted)
	scopedCompacted, err := compact.Compact(redact.AlreadyRedacted(v1Content.Transcript), compact.MetadataFields{
		Agent:      string(v1Content.Metadata.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  v1Content.Metadata.GetTranscriptStart(),
	})
	require.NoError(t, err)
	require.NotNil(t, scopedCompacted)
	require.Greater(t, bytes.Count(fullCompacted, []byte{'\n'}), bytes.Count(scopedCompacted, []byte{'\n'}))
	expectedOffset := computeCompactOffset(ctx, v1Content.Transcript, fullCompacted, v1Content.Metadata)
	require.Positive(t, expectedOffset, "expected non-zero compact transcript start")

	var stdout bytes.Buffer
	result, _, migrateErr := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	require.NoError(t, err)
	v2MainTree, err := v2MainCommit.Tree()
	require.NoError(t, err)

	metadataFile, err := v2MainTree.File(cpID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent, err := metadataFile.Contents()
	require.NoError(t, err)

	var metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &metadata))
	assert.Equal(t, expectedOffset, metadata.CheckpointTranscriptStart)

	storedCompact, err := v2Store.ReadSessionCompactTranscript(ctx, cpID, 0)
	require.NoError(t, err)
	assert.Equal(t, fullCompacted, storedCompact, "migration should persist cumulative compact transcript")
}

func TestMigrateCheckpointsV2_SkipsRepairWhenArchivedFullExists(t *testing.T) {
	oldMax := migrateMaxCheckpointsPerGeneration
	migrateMaxCheckpointsPerGeneration = 1
	t.Cleanup(func() {
		migrateMaxCheckpointsPerGeneration = oldMax
	})

	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("334455ddeeff")
	writeV1Checkpoint(t, v1Store, cpID, "session-repair-archive-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"repair from archive fallback\"}\n"),
		[]string{"repair archive prompt"},
	)

	// Initial migration to seed v2.
	var initialRun bytes.Buffer
	result1, _, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	// Fresh migration packs raw transcripts into an archived generation and
	// leaves /full/current empty.
	archivedRead, archivedReadErr := v2Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, archivedReadErr)
	assert.NotEmpty(t, archivedRead.Transcript)

	// Re-run migration: archived /full/* artifacts are sufficient, so it should
	// not rehydrate old raw transcripts into /full/current.
	var rerun bytes.Buffer
	result2, _, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &rerun, false)
	require.NoError(t, rerunErr)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
	assert.NotContains(t, rerun.String(), "repaired partial v2 checkpoint state")

	ok, checkErr := v2Store.HasFullSessionArtifacts(cpID, 0)
	require.NoError(t, checkErr)
	assert.True(t, ok, "expected archived /full/* artifacts to count as present")
	assert.False(t, hasCurrentFullSessionArtifactsForTest(t, repo, v2Store, cpID, 0),
		"migration rerun must not copy archived artifacts back into /full/current")
}

func v2FullTreeForCheckpoint(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID) *object.Tree {
	t.Helper()

	for _, refName := range v2FullRefSearchOrderForTest(t, v2Store) {
		_, rootTreeHash, err := v2Store.GetRefState(refName)
		if err != nil {
			continue
		}
		rootTree, err := repo.TreeObject(rootTreeHash)
		require.NoError(t, err)
		if _, treeErr := rootTree.Tree(cpID.Path()); treeErr == nil {
			return rootTree
		}
	}

	t.Fatalf("checkpoint %s not found in any v2 /full/* ref", cpID)
	return nil
}

func v2FullFileExistsForCheckpoint(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, relPath string) bool {
	t.Helper()

	for _, refName := range v2FullRefSearchOrderForTest(t, v2Store) {
		_, rootTreeHash, err := v2Store.GetRefState(refName)
		if err != nil {
			continue
		}
		rootTree, err := repo.TreeObject(rootTreeHash)
		require.NoError(t, err)
		if _, err := rootTree.File(cpID.Path() + "/" + relPath); err == nil {
			return true
		}
	}
	return false
}

// removeV2SessionTranscriptFiles deletes raw_transcript[/.NNN] and
// raw_transcript_hash.txt for the given session from every /full/* ref —
// simulating the partial state of an interrupted migration.
func removeV2SessionTranscriptFiles(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int) {
	t.Helper()

	for _, refName := range v2FullRefSearchOrderForTest(t, v2Store) {
		parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
		if err != nil {
			continue
		}

		newRootHash, updateErr := checkpoint.UpdateSubtree(
			repo,
			rootTreeHash,
			[]string{string(cpID[:2]), string(cpID[2:]), strconv.Itoa(sessionIdx)},
			nil,
			checkpoint.UpdateSubtreeOptions{
				MergeMode: checkpoint.MergeKeepExisting,
				DeleteNames: []string{
					paths.V2RawTranscriptFileName,
					paths.V2RawTranscriptFileName + ".001",
					paths.V2RawTranscriptFileName + ".002",
					paths.V2RawTranscriptHashFileName,
				},
			},
		)
		require.NoError(t, updateErr)
		if newRootHash == rootTreeHash {
			continue
		}

		commitHash, commitErr := checkpoint.CreateCommit(context.Background(), repo, newRootHash, parentHash,
			"test: remove full transcript\n", "Test", "test@test.com")
		require.NoError(t, commitErr)
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
	}
}

func v2FullRefSearchOrderForTest(t *testing.T, v2Store *checkpoint.V2GitStore) []plumbing.ReferenceName {
	t.Helper()

	refNames := []plumbing.ReferenceName{plumbing.ReferenceName(paths.V2FullCurrentRefName)}
	archived, err := v2Store.ListArchivedGenerations()
	require.NoError(t, err)
	for i := len(archived) - 1; i >= 0; i-- {
		refNames = append(refNames, plumbing.ReferenceName(paths.V2FullRefPrefix+archived[i]))
	}
	return refNames
}

func hasCurrentFullSessionArtifactsForTest(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int) bool {
	t.Helper()

	_, rootTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)

	rootTree, err := repo.TreeObject(rootTreeHash)
	require.NoError(t, err)

	sessionPath := cpID.Path() + "/" + strconv.Itoa(sessionIdx)
	sessionTree, err := rootTree.Tree(sessionPath)
	if err != nil {
		return false
	}

	hasTranscript := false
	for _, entry := range sessionTree.Entries {
		if entry.Name == paths.V2RawTranscriptFileName || strings.HasPrefix(entry.Name, paths.V2RawTranscriptFileName+".") {
			hasTranscript = true
			break
		}
	}
	if !hasTranscript {
		return false
	}

	_, err = sessionTree.File(paths.V2RawTranscriptHashFileName)
	return err == nil
}

func TestBuildMigrateWriteOpts_PromptSeparatorRoundTrip(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("123456abcdef")
	rawPrompts := strings.Join([]string{
		"first line\nwith newline",
		"second prompt",
	}, checkpoint.PromptSeparator)

	opts := buildMigrateWriteOpts(&checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID: "session-prompts-001",
			Strategy:  "manual-commit",
		},
		Prompts: rawPrompts,
	}, checkpoint.CommittedInfo{
		CheckpointID: cpID,
	}, nil)

	require.Len(t, opts.Prompts, 2)
	assert.Equal(t, "first line\nwith newline", opts.Prompts[0])
	assert.Equal(t, "second prompt", opts.Prompts[1])
}

func TestLatestMigratedV2SessionIndex_Empty(t *testing.T) {
	t.Parallel()

	latest, ok := latestMigratedV2SessionIndex(nil)
	assert.Equal(t, -1, latest)
	assert.False(t, ok)
}

func TestMigrateCheckpointsV2_PreservesPromptAttributions(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("aabb22334455")
	promptAttrs := json.RawMessage(`[{"prompt_index":0,"user_lines":["main.go:10"]}]`)

	err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:           cpID,
		SessionID:              "session-pa-001",
		Strategy:               "manual-commit",
		Transcript:             redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"pa test\"}\n")),
		Prompts:                []string{"test prompt"},
		PromptAttributionsJSON: promptAttrs,
		AuthorName:             "Test",
		AuthorEmail:            "test@test.com",
	})
	require.NoError(t, err)

	// Verify v1 has prompt_attributions
	v1Content, err := v1Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, v1Content.Metadata.PromptAttributions, "v1 should have prompt_attributions")

	// Migrate
	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	// Read v2 session metadata from /main ref and verify prompt_attributions preserved
	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	require.NoError(t, err)
	v2MainTree, err := v2MainCommit.Tree()
	require.NoError(t, err)

	metadataFile, err := v2MainTree.File(cpID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent, err := metadataFile.Contents()
	require.NoError(t, err)

	var metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &metadata))
	assert.JSONEq(t, string(promptAttrs), string(metadata.PromptAttributions),
		"v2 session metadata should preserve prompt_attributions from v1")
}

func TestMigrateCheckpointsV2_PreservesCombinedAttribution(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("ccdd55667788")

	// Write two sessions so combined attribution is meaningful
	writeV1Checkpoint(t, v1Store, cpID, "session-ca-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 1\"}\n"),
		[]string{"prompt 1"},
	)
	writeV1Checkpoint(t, v1Store, cpID, "session-ca-002",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 2\"}\n"),
		[]string{"prompt 2"},
	)

	// Inject CombinedAttribution into v1 root summary
	combined := &checkpoint.InitialAttribution{
		CalculatedAt:      time.Date(2026, 4, 15, 0, 18, 47, 0, time.UTC),
		AgentLines:        119,
		AgentRemoved:      94,
		HumanAdded:        3,
		HumanModified:     0,
		HumanRemoved:      1,
		TotalCommitted:    122,
		TotalLinesChanged: 217,
		AgentPercentage:   98.15668202764977,
		MetricVersion:     2,
	}
	err := v1Store.UpdateCheckpointSummary(ctx, cpID, combined)
	require.NoError(t, err)

	// Verify v1 root summary has CombinedAttribution
	v1Summary, err := v1Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, v1Summary.CombinedAttribution, "v1 should have combined_attribution")

	// Migrate
	var stdout bytes.Buffer
	result, _, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	// Read v2 root summary and verify CombinedAttribution preserved
	v2Summary, err := v2Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, v2Summary)
	require.NotNil(t, v2Summary.CombinedAttribution,
		"v2 root summary should preserve combined_attribution from v1")
	assert.Equal(t, combined.CalculatedAt, v2Summary.CombinedAttribution.CalculatedAt)
	assert.Equal(t, combined.AgentLines, v2Summary.CombinedAttribution.AgentLines)
	assert.Equal(t, combined.AgentRemoved, v2Summary.CombinedAttribution.AgentRemoved)
	assert.Equal(t, combined.HumanAdded, v2Summary.CombinedAttribution.HumanAdded)
	assert.Equal(t, combined.HumanModified, v2Summary.CombinedAttribution.HumanModified)
	assert.Equal(t, combined.HumanRemoved, v2Summary.CombinedAttribution.HumanRemoved)
	assert.Equal(t, combined.TotalCommitted, v2Summary.CombinedAttribution.TotalCommitted)
	assert.Equal(t, combined.TotalLinesChanged, v2Summary.CombinedAttribution.TotalLinesChanged)
	assert.InDelta(t, combined.AgentPercentage, v2Summary.CombinedAttribution.AgentPercentage, 0.001)
	assert.Equal(t, combined.MetricVersion, v2Summary.CombinedAttribution.MetricVersion)
}

func TestSortMigratableCheckpoints(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		input []checkpoint.CommittedInfo
		want  []id.CheckpointID
	}{
		{
			name: "chronological order",
			input: []checkpoint.CommittedInfo{
				{CheckpointID: id.MustCheckpointID("000000000003"), CreatedAt: t3},
				{CheckpointID: id.MustCheckpointID("000000000001"), CreatedAt: t1},
				{CheckpointID: id.MustCheckpointID("000000000002"), CreatedAt: t2},
			},
			want: []id.CheckpointID{
				id.MustCheckpointID("000000000001"),
				id.MustCheckpointID("000000000002"),
				id.MustCheckpointID("000000000003"),
			},
		},
		{
			name: "ties on CreatedAt break by checkpoint ID",
			input: []checkpoint.CommittedInfo{
				{CheckpointID: id.MustCheckpointID("0000000000bb"), CreatedAt: t1},
				{CheckpointID: id.MustCheckpointID("0000000000aa"), CreatedAt: t1},
				{CheckpointID: id.MustCheckpointID("0000000000cc"), CreatedAt: t1},
			},
			want: []id.CheckpointID{
				id.MustCheckpointID("0000000000aa"),
				id.MustCheckpointID("0000000000bb"),
				id.MustCheckpointID("0000000000cc"),
			},
		},
		{
			name: "zero CreatedAt sorts after non-zero, ties by ID",
			input: []checkpoint.CommittedInfo{
				{CheckpointID: id.MustCheckpointID("0000000000aa")},
				{CheckpointID: id.MustCheckpointID("000000000002"), CreatedAt: t2},
				{CheckpointID: id.MustCheckpointID("0000000000bb")},
				{CheckpointID: id.MustCheckpointID("000000000001"), CreatedAt: t1},
			},
			want: []id.CheckpointID{
				id.MustCheckpointID("000000000001"),
				id.MustCheckpointID("000000000002"),
				id.MustCheckpointID("0000000000aa"),
				id.MustCheckpointID("0000000000bb"),
			},
		},
		{
			name: "all-zero CreatedAt sorts by ID",
			input: []checkpoint.CommittedInfo{
				{CheckpointID: id.MustCheckpointID("0000000000cc")},
				{CheckpointID: id.MustCheckpointID("0000000000aa")},
				{CheckpointID: id.MustCheckpointID("0000000000bb")},
			},
			want: []id.CheckpointID{
				id.MustCheckpointID("0000000000aa"),
				id.MustCheckpointID("0000000000bb"),
				id.MustCheckpointID("0000000000cc"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := make([]checkpoint.CommittedInfo, len(tt.input))
			copy(input, tt.input)
			sortMigratableCheckpoints(input)
			got := make([]id.CheckpointID, len(input))
			for i, c := range input {
				got[i] = c.CheckpointID
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
