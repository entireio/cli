package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadGeneration_EmptyTree_ReturnsDefault(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	// Build an empty tree
	emptyTree, err := BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{})
	require.NoError(t, err)

	gen, err := store.ReadGeneration(emptyTree)
	require.NoError(t, err)

	assert.True(t, gen.OldestCheckpointAt.IsZero())
	assert.True(t, gen.NewestCheckpointAt.IsZero())
}

func TestReadGeneration_ParsesJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	now := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)
	original := GenerationMetadata{
		OldestCheckpointAt: now.Add(-1 * time.Hour),
		NewestCheckpointAt: now,
	}

	// Write generation.json into a tree
	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(original, entries))

	treeHash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)

	// Read it back
	gen, err := store.ReadGeneration(treeHash)
	require.NoError(t, err)

	assert.True(t, gen.OldestCheckpointAt.Equal(now.Add(-1*time.Hour)))
	assert.True(t, gen.NewestCheckpointAt.Equal(now))
}

func TestWriteGeneration_RoundTrips(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	original := GenerationMetadata{
		OldestCheckpointAt: now,
		NewestCheckpointAt: now,
	}

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(original, entries))

	// Verify the entry was added at the right key
	_, ok := entries[paths.GenerationFileName]
	assert.True(t, ok)

	// Build tree and read back
	treeHash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)

	gen, err := store.ReadGeneration(treeHash)
	require.NoError(t, err)

	assert.True(t, gen.OldestCheckpointAt.Equal(now))
	assert.True(t, gen.NewestCheckpointAt.Equal(now))
}

func TestReadGenerationFromRef(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	// Create a ref with generation.json in its tree
	now := time.Date(2026, 3, 25, 14, 0, 0, 0, time.UTC)
	gen := GenerationMetadata{
		OldestCheckpointAt: now,
		NewestCheckpointAt: now,
	}

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(gen, entries))
	treeHash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	commitHash, err := CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash, "test", authorName, authorEmail)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))

	// Read back via ref
	result, err := store.ReadGenerationFromRef(refName)
	require.NoError(t, err)

	assert.True(t, result.OldestCheckpointAt.Equal(now))
	assert.True(t, result.NewestCheckpointAt.Equal(now))
}

func TestAddGenerationJSONToTree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	// Start with a root tree that has a shard directory entry (simulating checkpoint data)
	shardEntries := map[string]object.TreeEntry{}
	shardEntries["aa/bbccddeeff/0/"+paths.V2RawTranscriptFileName] = object.TreeEntry{
		Name: paths.V2RawTranscriptFileName,
		Mode: 0o100644,
		Hash: plumbing.ZeroHash, // dummy
	}
	rootTreeHash, err := BuildTreeFromEntries(context.Background(), repo, shardEntries)
	require.NoError(t, err)

	gen := GenerationMetadata{
		OldestCheckpointAt: time.Now().UTC(),
		NewestCheckpointAt: time.Now().UTC(),
	}

	// Add generation.json to the root tree
	newRootHash, err := store.AddGenerationJSONToTree(rootTreeHash, gen)
	require.NoError(t, err)
	assert.NotEqual(t, rootTreeHash, newRootHash)

	// Verify generation.json is present and shard dir is preserved
	readGen, err := store.ReadGeneration(newRootHash)
	require.NoError(t, err)
	assert.False(t, readGen.OldestCheckpointAt.IsZero())

	// Verify the shard directory still exists in the tree
	tree, err := repo.TreeObject(newRootHash)
	require.NoError(t, err)
	foundShard := false
	for _, e := range tree.Entries {
		if e.Name == "aa" {
			foundShard = true
		}
	}
	assert.True(t, foundShard, "shard directory should be preserved")
}

func TestCountCheckpointsInTree_EmptyTree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	count, err := store.CountCheckpointsInTree(plumbing.ZeroHash)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestCountCheckpointsInTree_CountsShardDirectories(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	// Write 3 checkpoints to /full/current
	cpIDs := []id.CheckpointID{
		id.MustCheckpointID("aabbccddeeff"),
		id.MustCheckpointID("112233445566"),
		id.MustCheckpointID("ffeeddccbbaa"),
	}

	for _, cpID := range cpIDs {
		err := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    "test-session",
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   redact.AlreadyRedacted([]byte(`{"type":"test"}`)),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	_, treeHash, err := store.GetRefState(refName)
	require.NoError(t, err)

	count, err := store.CountCheckpointsInTree(treeHash)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestWriteCommittedFull_NoGenerationJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("d1e2f3a4b5c6")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-gen-001",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"hello"}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// /full/current should NOT contain generation.json (written at archive time only)
	fullTree := v2FullTree(t, repo)
	for _, entry := range fullTree.Entries {
		assert.NotEqual(t, paths.GenerationFileName, entry.Name,
			"/full/current should not contain generation.json")
	}

	// Checkpoint data should still be present
	content := v2ReadFile(t, fullTree, cpID.Path()+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, content, "hello")
}

func TestUpdateCommitted_DoesNotAddGenerationJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("a4b5c6d1e2f3")

	// Initial write
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-noupdate-gen",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"initial"}`)),
		Prompts:      []string{"first"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Update (stop-time finalization)
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-noupdate-gen",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"finalized"}`)),
		Prompts:      []string{"first", "second"},
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	// /full/current should still not have generation.json
	fullTree := v2FullTree(t, repo)
	for _, entry := range fullTree.Entries {
		assert.NotEqual(t, paths.GenerationFileName, entry.Name,
			"/full/current should not contain generation.json after update")
	}

	// Verify the transcript was actually updated (sanity check)
	content := v2ReadFile(t, fullTree, cpID.Path()+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, content, "finalized")
}

// createArchivedRef creates a dummy archived generation ref for testing.
func createArchivedRef(t *testing.T, repo *git.Repository, number int) {
	t.Helper()
	store := NewV2GitStore(repo, "origin")

	// Build a minimal tree with just generation.json
	now := time.Now().UTC()
	gen := GenerationMetadata{
		OldestCheckpointAt: now.Add(-time.Hour),
		NewestCheckpointAt: now,
	}
	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(gen, entries))
	treeHash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)

	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	commitHash, err := CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash, "archived", authorName, authorEmail)
	require.NoError(t, err)

	refName := ArchivedGenerationRefName(number)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func TestArchivedGenerationRefName(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		plumbing.ReferenceName(paths.V2FullRefPrefix+"0000000000001"),
		ArchivedGenerationRefName(1),
	)
}

func TestListArchivedGenerations_Empty(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Empty(t, archived)
}

func TestListArchivedGenerations_FindsArchived(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	createArchivedRef(t, repo, 1)
	createArchivedRef(t, repo, 2)

	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000001", "0000000000002"}, archived)
}

func TestListArchivedGenerations_ExcludesCurrent(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	// Create /full/current ref
	require.NoError(t, store.ensureRef(context.Background(), plumbing.ReferenceName(paths.V2FullCurrentRefName)))

	// Create an archived ref
	createArchivedRef(t, repo, 1)

	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000001"}, archived)
}

func TestNextGenerationNumber_NoArchives(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	next, err := store.NextGenerationNumber()
	require.NoError(t, err)
	assert.Equal(t, 1, next)
}

func TestNextGenerationNumber_WithExisting(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	createArchivedRef(t, repo, 1)
	createArchivedRef(t, repo, 2)

	next, err := store.NextGenerationNumber()
	require.NoError(t, err)
	assert.Equal(t, 3, next)
}

// populateFullCurrent writes n checkpoints to /full/current via WriteCommitted.
// offset shifts the generated checkpoint IDs to avoid collisions across calls.
func populateFullCurrent(t *testing.T, store *V2GitStore, n, offset int) []id.CheckpointID {
	t.Helper()
	ctx := context.Background()
	cpIDs := make([]id.CheckpointID, n)
	for i := range n {
		cpIDs[i] = id.MustCheckpointID(fmt.Sprintf("%012x", offset+i+1))
		err := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpIDs[i],
			SessionID:    fmt.Sprintf("session-rot-%d", offset+i),
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   redact.AlreadyRedacted([]byte(fmt.Sprintf(`{"cp":%d}`, i))),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}
	return cpIDs
}

func TestRotateGeneration_ArchivesCurrentAndCreatesNewOrphan(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 3

	// Write 3 checkpoints — the 3rd triggers auto-rotation via writeCommittedFullTranscript
	cpIDs := populateFullCurrent(t, store, 3, 0)

	// --- Verify archived ref ---
	archiveRefName := ArchivedGenerationRefName(1)
	archiveRef, err := repo.Reference(archiveRefName, true)
	require.NoError(t, err, "archived ref should exist")

	// Archived ref should contain generation.json with timestamps
	archiveCommit, err := repo.CommitObject(archiveRef.Hash())
	require.NoError(t, err)
	archiveGen, err := store.ReadGeneration(archiveCommit.TreeHash)
	require.NoError(t, err)
	assert.False(t, archiveGen.OldestCheckpointAt.IsZero(), "archived generation should have oldest timestamp")
	assert.False(t, archiveGen.NewestCheckpointAt.IsZero(), "archived generation should have newest timestamp")

	// Archived tree should contain the checkpoint data
	archiveTree, err := archiveCommit.Tree()
	require.NoError(t, err)
	for _, cpID := range cpIDs {
		_, treeErr := archiveTree.File(cpID.Path() + "/0/" + paths.V2RawTranscriptFileName)
		require.NoError(t, treeErr, "archived tree should contain transcript for %s", cpID)
	}

	// Archived tree should also contain generation.json
	_, genErr := archiveTree.File(paths.GenerationFileName)
	require.NoError(t, genErr, "archived tree should contain generation.json")

	// --- Verify fresh /full/current ---
	fullRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)
	freshCommit, err := repo.CommitObject(fullRef.Hash())
	require.NoError(t, err)

	// Fresh commit should be an orphan (no parents)
	assert.Empty(t, freshCommit.ParentHashes, "fresh /full/current should be an orphan commit")

	// Fresh tree should be empty (no generation.json, no shard directories)
	freshTree, err := freshCommit.Tree()
	require.NoError(t, err)
	assert.Empty(t, freshTree.Entries, "fresh tree should be empty (no generation.json)")
}

func TestResetFullCurrentRefIfUnchangedRejectsConcurrentChange(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	treeHash, err := BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	baseCommit, err := CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		"base current\n", "Test", "test@test.com")
	require.NoError(t, err)
	concurrentCommit, err := CreateCommit(ctx, repo, treeHash, baseCommit,
		"concurrent current\n", "Test", "test@test.com")
	require.NoError(t, err)
	orphanCommit, err := CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		"Start generation\n", "Test", "test@test.com")
	require.NoError(t, err)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	expectedRef := plumbing.NewHashReference(refName, baseCommit)
	require.NoError(t, repo.Storer.SetReference(expectedRef))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, concurrentCommit)))

	reset, err := store.resetFullCurrentRefIfUnchanged(ctx, refName, expectedRef, orphanCommit)
	require.NoError(t, err)
	assert.False(t, reset)

	currentRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	assert.Equal(t, concurrentCommit, currentRef.Hash())
}

func TestRotateGeneration_UsesCheckpointCreatedAt(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 2
	ctx := context.Background()

	oldest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	newest := time.Date(2026, 1, 5, 6, 7, 8, 0, time.UTC)

	for i, createdAt := range []time.Time{oldest, newest} {
		err := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: id.MustCheckpointID(fmt.Sprintf("%012x", i+1)),
			SessionID:    fmt.Sprintf("session-created-at-%d", i),
			CreatedAt:    createdAt,
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   redact.AlreadyRedacted([]byte(fmt.Sprintf(`{"type":"assistant","timestamp":%q}`, createdAt.Format(time.RFC3339Nano)))),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}

	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Len(t, archived, 1)

	gen, err := store.ReadGenerationFromRef(plumbing.ReferenceName(paths.V2FullRefPrefix + archived[0]))
	require.NoError(t, err)
	assert.True(t, gen.OldestCheckpointAt.Equal(oldest), "oldest should come from checkpoint metadata")
	assert.True(t, gen.NewestCheckpointAt.Equal(newest), "newest should come from checkpoint metadata")
}

func TestComputeGenerationCheckpointTimestamps_FallsBackToRawTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	oldest := time.Date(2025, 12, 23, 10, 27, 44, 0, time.UTC)
	newest := time.Date(2025, 12, 23, 10, 31, 37, 0, time.UTC)
	transcript := fmt.Sprintf(
		"{\"type\":\"user\",\"timestamp\":%q}\n{\"type\":\"assistant\",\"timestamp\":%q}\n",
		oldest.Format(time.RFC3339Nano),
		newest.Format(time.RFC3339Nano),
	)
	blobHash, err := CreateBlobFromContent(repo, []byte(transcript))
	require.NoError(t, err)

	cpID := id.MustCheckpointID("aabbccddeeff")
	rootTreeHash, err := BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		cpID.Path() + "/0/" + paths.V2RawTranscriptFileName: {
			Name: paths.V2RawTranscriptFileName,
			Mode: 0o100644,
			Hash: blobHash,
		},
	})
	require.NoError(t, err)

	gen, ok, err := store.ComputeGenerationCheckpointTimestamps(rootTreeHash)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, gen.OldestCheckpointAt.Equal(oldest))
	assert.True(t, gen.NewestCheckpointAt.Equal(newest))
}

func TestComputeGenerationTimestampsFromTrees_IgnoresMainMetadataWhenNil(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	cpID := id.MustCheckpointID("aabbccddeeff")
	mainCreatedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-main-created-at",
		CreatedAt:    mainCreatedAt,
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(fmt.Sprintf(`{"type":"assistant","timestamp":%q}`, mainCreatedAt.Format(time.RFC3339Nano)))),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	rawOldest := time.Date(2025, 12, 23, 10, 27, 44, 0, time.UTC)
	rawNewest := time.Date(2025, 12, 23, 10, 31, 37, 0, time.UTC)
	transcript := fmt.Sprintf(
		"{\"type\":\"user\",\"timestamp\":%q}\n{\"type\":\"assistant\",\"timestamp\":%q}\n",
		rawOldest.Format(time.RFC3339Nano),
		rawNewest.Format(time.RFC3339Nano),
	)
	blobHash, err := CreateBlobFromContent(repo, []byte(transcript))
	require.NoError(t, err)

	rootTreeHash, err := BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		cpID.Path() + "/0/" + paths.V2RawTranscriptFileName: {
			Name: paths.V2RawTranscriptFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		},
	})
	require.NoError(t, err)

	gen, ok, err := store.ComputeGenerationTimestampsFromTrees(rootTreeHash, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, gen.OldestCheckpointAt.Equal(rawOldest))
	assert.True(t, gen.NewestCheckpointAt.Equal(rawNewest))
}

func TestComputeGenerationCheckpointTimestamps_UnreadableCheckpointForcesFallback(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	timestamp := time.Date(2025, 12, 23, 10, 27, 44, 0, time.UTC)
	transcript := fmt.Sprintf("{\"type\":\"user\",\"timestamp\":%q}\n", timestamp.Format(time.RFC3339Nano))
	transcriptBlobHash, err := CreateBlobFromContent(repo, []byte(transcript))
	require.NoError(t, err)

	readableCheckpointTree, err := BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		"0/" + paths.V2RawTranscriptFileName: {
			Name: paths.V2RawTranscriptFileName,
			Mode: filemode.Regular,
			Hash: transcriptBlobHash,
		},
	})
	require.NoError(t, err)

	bucketTree, err := storeTree(repo, []object.TreeEntry{
		{
			Name: "bbccddeeff",
			Mode: filemode.Dir,
			Hash: readableCheckpointTree,
		},
		{
			Name: "ccddeeff00",
			Mode: filemode.Dir,
			Hash: plumbing.NewHash("1111111111111111111111111111111111111111"),
		},
	})
	require.NoError(t, err)

	rootTreeHash, err := storeTree(repo, []object.TreeEntry{
		{
			Name: "aa",
			Mode: filemode.Dir,
			Hash: bucketTree,
		},
	})
	require.NoError(t, err)

	gen, ok, err := store.ComputeGenerationCheckpointTimestamps(rootTreeHash)
	require.NoError(t, err)
	assert.False(t, ok, "partial checkpoint timestamp coverage should force fallback")
	assert.True(t, gen.OldestCheckpointAt.IsZero())
	assert.True(t, gen.NewestCheckpointAt.IsZero())
}

func TestUpdateCommittedFullTranscript_UpdatesArchivedGeneration(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 1
	ctx := context.Background()

	cpID := id.MustCheckpointID("abc123def456")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-archived-update",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"provisional"}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	require.Len(t, archived, 1)

	_, currentTreeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	currentCount, err := store.CountCheckpointsInTree(currentTreeHash)
	require.NoError(t, err)
	require.Equal(t, 0, currentCount, "rotation should leave /full/current empty")

	finalTranscript := redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"final"}`))
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-archived-update",
		Transcript:   finalTranscript,
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	_, currentTreeHash, err = store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	currentCount, err = store.CountCheckpointsInTree(currentTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 0, currentCount, "finalization must not rehydrate archived checkpoints into /full/current")

	_, archiveTreeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2FullRefPrefix + archived[0]))
	require.NoError(t, err)
	archiveTree, err := repo.TreeObject(archiveTreeHash)
	require.NoError(t, err)
	got := v2ReadFile(t, archiveTree, cpID.Path()+"/0/"+paths.V2RawTranscriptFileName)
	assert.Equal(t, string(finalTranscript.Bytes()), got)
}

func TestRotateGeneration_SequentialNumbering(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 2
	ctx := context.Background()

	// First rotation: populate and rotate
	populateFullCurrent(t, store, 2, 0)
	require.NoError(t, store.rotateGeneration(ctx))

	// Second rotation: populate with different IDs and rotate
	populateFullCurrent(t, store, 2, 100)
	require.NoError(t, store.rotateGeneration(ctx))

	// Verify both archived refs exist with correct generation numbers
	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000001", "0000000000002"}, archived)

	// Verify each archived ref has generation.json with timestamps
	for _, name := range archived {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + name)
		gen, readErr := store.ReadGenerationFromRef(refName)
		require.NoError(t, readErr)
		assert.False(t, gen.OldestCheckpointAt.IsZero(), "archive %s should have oldest timestamp", name)
		assert.False(t, gen.NewestCheckpointAt.IsZero(), "archive %s should have newest timestamp", name)

		// Verify checkpoint count via tree walk
		_, treeHash, refErr := store.GetRefState(refName)
		require.NoError(t, refErr)
		count, countErr := store.CountCheckpointsInTree(treeHash)
		require.NoError(t, countErr)
		assert.Equal(t, 2, count, "archive %s should have 2 checkpoints", name)
	}
}

// Verify generation.json is correctly read from old format (with checkpoints field).
// This ensures backward compatibility when reading archived generations created
// before the Checkpoints field was removed.
func TestReadGeneration_BackwardCompatible(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	// Simulate old format with a checkpoints field
	oldJSON := `{
		"checkpoints": ["aabbccddeeff", "112233445566"],
		"oldest_checkpoint_at": "2026-03-25T11:00:00Z",
		"newest_checkpoint_at": "2026-03-25T12:00:00Z"
	}`
	blobHash, err := CreateBlobFromContent(repo, []byte(oldJSON))
	require.NoError(t, err)

	entries := map[string]object.TreeEntry{
		paths.GenerationFileName: {
			Name: paths.GenerationFileName,
			Mode: 0o100644,
			Hash: blobHash,
		},
	}
	treeHash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)

	// Should parse without error, ignoring the unknown checkpoints field
	gen, err := store.ReadGeneration(treeHash)
	require.NoError(t, err)

	expected := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)
	assert.True(t, gen.NewestCheckpointAt.Equal(expected))
}

// Verify backward-compatible JSON encoding: old data with "checkpoints" key
// should still parse (JSON ignores unknown fields by default).
func TestGenerationMetadata_JSONBackwardCompat(t *testing.T) {
	t.Parallel()

	oldJSON := `{"checkpoints":["aabbccddeeff"],"oldest_checkpoint_at":"2026-01-01T00:00:00Z","newest_checkpoint_at":"2026-02-01T00:00:00Z"}`
	var gen GenerationMetadata
	err := json.Unmarshal([]byte(oldJSON), &gen)
	require.NoError(t, err)
	assert.False(t, gen.OldestCheckpointAt.IsZero())
	assert.False(t, gen.NewestCheckpointAt.IsZero())
}
