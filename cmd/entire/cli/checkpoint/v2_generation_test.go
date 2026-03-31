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
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadGeneration_EmptyTree_ReturnsDefault(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	// Build an empty tree
	emptyTree, err := BuildTreeFromEntries(repo, map[string]object.TreeEntry{})
	require.NoError(t, err)

	gen, err := store.readGeneration(emptyTree)
	require.NoError(t, err)

	assert.Empty(t, gen.Checkpoints)
	assert.True(t, gen.OldestCheckpointAt.IsZero())
	assert.True(t, gen.NewestCheckpointAt.IsZero())
}

func TestReadGeneration_ParsesJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	now := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)
	original := GenerationMetadata{
		Checkpoints:        []id.CheckpointID{id.MustCheckpointID("aabbccddeeff"), id.MustCheckpointID("112233445566")},
		OldestCheckpointAt: now.Add(-1 * time.Hour),
		NewestCheckpointAt: now,
	}

	// Write generation.json into a tree
	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(original, entries))

	treeHash, err := BuildTreeFromEntries(repo, entries)
	require.NoError(t, err)

	// Read it back
	gen, err := store.readGeneration(treeHash)
	require.NoError(t, err)

	assert.Equal(t, []id.CheckpointID{id.MustCheckpointID("aabbccddeeff"), id.MustCheckpointID("112233445566")}, gen.Checkpoints)
	assert.True(t, gen.OldestCheckpointAt.Equal(now.Add(-1*time.Hour)))
	assert.True(t, gen.NewestCheckpointAt.Equal(now))
}

func TestWriteGeneration_RoundTrips(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	original := GenerationMetadata{
		Checkpoints:        []id.CheckpointID{id.MustCheckpointID("aabbccddeeff")},
		OldestCheckpointAt: now,
		NewestCheckpointAt: now,
	}

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(original, entries))

	// Verify the entry was added at the right key
	_, ok := entries[paths.GenerationFileName]
	assert.True(t, ok)

	// Build tree and read back
	treeHash, err := BuildTreeFromEntries(repo, entries)
	require.NoError(t, err)

	gen, err := store.readGeneration(treeHash)
	require.NoError(t, err)

	assert.Equal(t, original.Checkpoints, gen.Checkpoints)
}

func TestReadGenerationFromRef(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	// Create a ref with generation.json in its tree
	now := time.Date(2026, 3, 25, 14, 0, 0, 0, time.UTC)
	gen := GenerationMetadata{
		Checkpoints:        []id.CheckpointID{id.MustCheckpointID("aabbccddeeff")},
		OldestCheckpointAt: now,
		NewestCheckpointAt: now,
	}

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(gen, entries))
	treeHash, err := BuildTreeFromEntries(repo, entries)
	require.NoError(t, err)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	commitHash, err := CreateCommit(repo, treeHash, plumbing.ZeroHash, "test", authorName, authorEmail)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))

	// Read back via ref
	result, err := store.readGenerationFromRef(refName)
	require.NoError(t, err)

	assert.Equal(t, []id.CheckpointID{id.MustCheckpointID("aabbccddeeff")}, result.Checkpoints)
}

func TestAddGenerationToRootTree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	// Start with a root tree that has a shard directory entry (simulating checkpoint data)
	shardEntries := map[string]object.TreeEntry{}
	shardEntries["aa/bbccddeeff/0/full.jsonl"] = object.TreeEntry{
		Name: "full.jsonl",
		Mode: 0o100644,
		Hash: plumbing.ZeroHash, // dummy
	}
	rootTreeHash, err := BuildTreeFromEntries(repo, shardEntries)
	require.NoError(t, err)

	gen := GenerationMetadata{
		Checkpoints: []id.CheckpointID{id.MustCheckpointID("aabbccddeeff")},
	}

	// Add generation.json to the root tree
	newRootHash, err := store.addGenerationToRootTree(rootTreeHash, gen)
	require.NoError(t, err)
	assert.NotEqual(t, rootTreeHash, newRootHash)

	// Verify generation.json is present and shard dir is preserved
	readGen, err := store.readGeneration(newRootHash)
	require.NoError(t, err)
	assert.Len(t, readGen.Checkpoints, 1)

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

// v2FullGeneration reads generation.json from the /full/current ref.
func v2FullGeneration(t *testing.T, repo *git.Repository) GenerationMetadata {
	t.Helper()
	store := NewV2GitStore(repo)
	gen, err := store.readGenerationFromRef(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	return gen
}

func TestWriteCommittedFull_UpdatesGenerationJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("d1e2f3a4b5c6")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-gen-001",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte(`{"type":"assistant","message":"hello"}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	gen := v2FullGeneration(t, repo)
	assert.Len(t, gen.Checkpoints, 1)
	assert.Equal(t, []id.CheckpointID{cpID}, gen.Checkpoints)
	assert.False(t, gen.OldestCheckpointAt.IsZero())
	assert.False(t, gen.NewestCheckpointAt.IsZero())
}

func TestWriteCommittedFull_AccumulatesInGenerationJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	cpA := id.MustCheckpointID("e2f3a4b5c6d1")
	cpB := id.MustCheckpointID("f3a4b5c6d1e2")

	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpA,
		SessionID:    "session-acc-A",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte(`{"from":"A"}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpB,
		SessionID:    "session-acc-B",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte(`{"from":"B"}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	gen := v2FullGeneration(t, repo)
	assert.Len(t, gen.Checkpoints, 2)
	assert.Equal(t, []id.CheckpointID{cpA, cpB}, gen.Checkpoints)
	assert.True(t, gen.NewestCheckpointAt.After(gen.OldestCheckpointAt) || gen.NewestCheckpointAt.Equal(gen.OldestCheckpointAt))
}

func TestUpdateCommitted_DoesNotUpdateGenerationJSON(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("a4b5c6d1e2f3")

	// Initial write
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-noupdate-gen",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte(`{"type":"assistant","message":"initial"}`),
		Prompts:      []string{"first"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	genBefore := v2FullGeneration(t, repo)
	require.Len(t, genBefore.Checkpoints, 1)

	// Update (stop-time finalization) — should NOT change generation.json
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-noupdate-gen",
		Transcript:   []byte(`{"type":"assistant","message":"finalized"}`),
		Prompts:      []string{"first", "second"},
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	genAfter := v2FullGeneration(t, repo)
	assert.Len(t, genAfter.Checkpoints, 1, "UpdateCommitted should not change checkpoint count")
	assert.Equal(t, genBefore.Checkpoints, genAfter.Checkpoints)

	// Verify the transcript was actually updated (sanity check)
	fullTree := v2FullTree(t, repo)
	content := v2ReadFile(t, fullTree, cpID.Path()+"/0/"+paths.TranscriptFileName)
	assert.Contains(t, content, "finalized")
}

func TestWriteCommittedFull_GenerationJSON_SameCheckpointIdNotDuplicated(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("b5c6d1e2f3a4")

	// Write same checkpoint twice (e.g., two sessions for the same commit)
	for _, sessID := range []string{"session-dup-1", "session-dup-2"} {
		err := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    sessID,
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   []byte(`{"from":"` + sessID + `"}`),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}

	gen := v2FullGeneration(t, repo)
	// Same checkpoint ID written twice should only appear once in the array
	assert.Len(t, gen.Checkpoints, 1)
	assert.Equal(t, []id.CheckpointID{cpID}, gen.Checkpoints)
}

func TestWriteCommittedFull_GenerationJSON_PreservedInTree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("c6d1e2f3a4b5")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-tree-check",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte(`{"check":"tree"}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Read the /full/current tree and verify generation.json is at root
	fullTree := v2FullTree(t, repo)
	genContent := v2ReadFile(t, fullTree, paths.GenerationFileName)
	var gen GenerationMetadata
	require.NoError(t, json.Unmarshal([]byte(genContent), &gen))
	assert.Len(t, gen.Checkpoints, 1)

	// Verify checkpoint data is also present
	content := v2ReadFile(t, fullTree, cpID.Path()+"/0/"+paths.TranscriptFileName)
	assert.Contains(t, content, `"check":"tree"`)
}

// createArchivedRef creates a dummy archived generation ref for testing.
func createArchivedRef(t *testing.T, repo *git.Repository, number int) {
	t.Helper()
	store := NewV2GitStore(repo)

	// Build a minimal tree with just generation.json
	gen := GenerationMetadata{
		Checkpoints: []id.CheckpointID{id.MustCheckpointID("d00000000000")},
	}
	entries := make(map[string]object.TreeEntry)
	require.NoError(t, store.writeGeneration(gen, entries))
	treeHash, err := BuildTreeFromEntries(repo, entries)
	require.NoError(t, err)

	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	commitHash, err := CreateCommit(repo, treeHash, plumbing.ZeroHash, "archived", authorName, authorEmail)
	require.NoError(t, err)

	refName := plumbing.ReferenceName(fmt.Sprintf("%s%013d", paths.V2FullRefPrefix, number))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func TestListArchivedGenerations_Empty(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	archived, err := store.listArchivedGenerations()
	require.NoError(t, err)
	assert.Empty(t, archived)
}

func TestListArchivedGenerations_FindsArchived(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	createArchivedRef(t, repo, 1)
	createArchivedRef(t, repo, 2)

	archived, err := store.listArchivedGenerations()
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000001", "0000000000002"}, archived)
}

func TestListArchivedGenerations_ExcludesCurrent(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	// Create /full/current ref
	require.NoError(t, store.ensureRef(plumbing.ReferenceName(paths.V2FullCurrentRefName)))

	// Create an archived ref
	createArchivedRef(t, repo, 1)

	archived, err := store.listArchivedGenerations()
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000001"}, archived)
}

func TestNextGenerationNumber_NoArchives(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	next, err := store.nextGenerationNumber()
	require.NoError(t, err)
	assert.Equal(t, 1, next)
}

func TestNextGenerationNumber_WithExisting(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	createArchivedRef(t, repo, 1)
	createArchivedRef(t, repo, 2)

	next, err := store.nextGenerationNumber()
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
			Transcript:   []byte(fmt.Sprintf(`{"cp":%d}`, i)),
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
	store := NewV2GitStore(repo)
	store.maxCheckpointsPerGeneration = 3

	// Write 3 checkpoints — the 3rd triggers auto-rotation via writeCommittedFullTranscript
	cpIDs := populateFullCurrent(t, store, 3, 0)

	// --- Verify archived ref ---
	archiveRefName := fmt.Sprintf("%s%013d", paths.V2FullRefPrefix, 1)
	archiveRef, err := repo.Reference(plumbing.ReferenceName(archiveRefName), true)
	require.NoError(t, err, "archived ref should exist")

	// Archived ref should contain all 3 checkpoints
	archiveCommit, err := repo.CommitObject(archiveRef.Hash())
	require.NoError(t, err)
	archiveGen, err := store.readGeneration(archiveCommit.TreeHash)
	require.NoError(t, err)
	assert.Len(t, archiveGen.Checkpoints, 3)
	for i, cpID := range cpIDs {
		assert.Equal(t, cpID, archiveGen.Checkpoints[i])
	}

	// Archived tree should contain the checkpoint data
	archiveTree, err := archiveCommit.Tree()
	require.NoError(t, err)
	for _, cpID := range cpIDs {
		_, treeErr := archiveTree.File(cpID.Path() + "/0/" + paths.TranscriptFileName)
		require.NoError(t, treeErr, "archived tree should contain transcript for %s", cpID)
	}

	// --- Verify fresh /full/current ---
	fullRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)
	freshCommit, err := repo.CommitObject(fullRef.Hash())
	require.NoError(t, err)

	// Fresh commit should be an orphan (no parents)
	assert.Empty(t, freshCommit.ParentHashes, "fresh /full/current should be an orphan commit")

	// Fresh tree should contain only generation.json (no shard directories)
	freshTree, err := freshCommit.Tree()
	require.NoError(t, err)
	assert.Len(t, freshTree.Entries, 1, "fresh tree should contain only generation.json")
	assert.Equal(t, paths.GenerationFileName, freshTree.Entries[0].Name)

	// Seed generation.json should have empty checkpoints
	freshGen, err := store.readGeneration(freshCommit.TreeHash)
	require.NoError(t, err)
	assert.Empty(t, freshGen.Checkpoints)
}

func TestRotateGeneration_SequentialNumbering(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	store.maxCheckpointsPerGeneration = 2
	ctx := context.Background()

	// First rotation: populate and rotate
	populateFullCurrent(t, store, 2, 0)
	require.NoError(t, store.rotateGeneration(ctx))

	// Second rotation: populate with different IDs and rotate
	populateFullCurrent(t, store, 2, 100)
	require.NoError(t, store.rotateGeneration(ctx))

	// Verify both archived refs exist with correct generation numbers
	archived, err := store.listArchivedGenerations()
	require.NoError(t, err)
	assert.Equal(t, []string{"0000000000001", "0000000000002"}, archived)

	// Verify each archived ref has checkpoints
	for _, name := range archived {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + name)
		gen, readErr := store.readGenerationFromRef(refName)
		require.NoError(t, readErr)
		assert.Len(t, gen.Checkpoints, 2, "archive %s should have 2 checkpoints", name)
	}
}
