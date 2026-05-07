package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// initTestRepo creates a bare-minimum git repo with one commit (needed for HEAD).
func initTestRepo(t *testing.T) *git.Repository {
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

func TestNewV2GitStore(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	require.NotNil(t, store)
	require.Equal(t, repo, store.repo)
}

func TestV2GitStore_EnsureRef_CreatesNewRef(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	refName := plumbing.ReferenceName(paths.V2MainRefName)

	// Ref should not exist yet
	_, err := repo.Reference(refName, true)
	require.Error(t, err)

	// Ensure creates it
	require.NoError(t, store.ensureRef(context.Background(), refName))

	// Ref should now exist and point to a valid commit with an empty tree
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	tree, err := commit.Tree()
	require.NoError(t, err)
	require.Empty(t, tree.Entries, "initial tree should be empty")
}

func TestV2GitStore_EnsureRef_Idempotent(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	refName := plumbing.ReferenceName(paths.V2MainRefName)

	require.NoError(t, store.ensureRef(context.Background(), refName))
	ref1, err := repo.Reference(refName, true)
	require.NoError(t, err)

	// Second call should be a no-op — same commit hash
	require.NoError(t, store.ensureRef(context.Background(), refName))
	ref2, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.Equal(t, ref1.Hash(), ref2.Hash())
}

func TestV2GitStore_EnsureRef_DifferentRefs(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	mainRef := plumbing.ReferenceName(paths.V2MainRefName)
	fullRef := plumbing.ReferenceName(paths.V2FullCurrentRefName)

	require.NoError(t, store.ensureRef(context.Background(), mainRef))
	require.NoError(t, store.ensureRef(context.Background(), fullRef))

	// Both should exist independently
	_, err := repo.Reference(mainRef, true)
	require.NoError(t, err)
	_, err = repo.Reference(fullRef, true)
	require.NoError(t, err)
}

func TestV2GitStore_GetRefState_ReturnsParentAndTree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	require.NoError(t, store.ensureRef(context.Background(), refName))

	parentHash, treeHash, err := store.GetRefState(refName)
	require.NoError(t, err)
	require.NotEqual(t, plumbing.ZeroHash, parentHash, "parent hash should be non-zero")
	// Tree hash can be zero hash for empty tree or a valid hash — just verify no error
	_ = treeHash
}

func TestV2GitStore_GetRefState_ErrorsOnMissingRef(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	refName := plumbing.ReferenceName("refs/entire/nonexistent")
	_, _, err := store.GetRefState(refName)
	require.Error(t, err)
}

func TestV2GitStore_UpdateRef_CreatesCommit(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	require.NoError(t, store.ensureRef(context.Background(), refName))

	parentHash, treeHash, err := store.GetRefState(refName)
	require.NoError(t, err)

	// Build a tree with one file
	blobHash, err := CreateBlobFromContent(repo, []byte("hello"))
	require.NoError(t, err)

	entries := map[string]object.TreeEntry{
		"test.txt": {Name: "test.txt", Mode: 0o100644, Hash: blobHash},
	}
	newTreeHash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)
	require.NotEqual(t, treeHash, newTreeHash)

	// Update the ref
	require.NoError(t, store.updateRef(context.Background(), refName, newTreeHash, parentHash, "test commit", "Test", "test@test.com"))

	// Verify the ref now points to a commit with our tree
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.NotEqual(t, parentHash, ref.Hash(), "ref should point to new commit")

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.Equal(t, newTreeHash, commit.TreeHash)
	require.Equal(t, "test commit", commit.Message)
	require.Len(t, commit.ParentHashes, 1)
	require.Equal(t, parentHash, commit.ParentHashes[0])
}

// v2MainTree returns the root tree from the /main ref for test assertions.
func v2MainTree(t *testing.T, repo *git.Repository) *object.Tree {
	t.Helper()
	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	return tree
}

// v2ReadFile reads a file from a git tree by path.
func v2ReadFile(t *testing.T, tree *object.Tree, path string) string {
	t.Helper()
	file, err := tree.File(path)
	require.NoError(t, err, "expected file at %s", path)
	content, err := file.Contents()
	require.NoError(t, err)
	return content
}

func TestV2GitStore_WriteCommittedMain_WritesMetadata(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-001",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"human","message":"hello"}`)),
		Prompts:      []string{"hello"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// Root CheckpointSummary should exist
	summaryContent := v2ReadFile(t, tree, cpPath+"/"+paths.MetadataFileName)
	var summary CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	assert.Equal(t, cpID, summary.CheckpointID)
	assert.Equal(t, "manual-commit", summary.Strategy)
	assert.Len(t, summary.Sessions, 1)

	// Session metadata should exist in subdirectory 0/
	sessionMeta := v2ReadFile(t, tree, cpPath+"/0/"+paths.MetadataFileName)
	var meta CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(sessionMeta), &meta))
	assert.Equal(t, "test-session-001", meta.SessionID)
	assert.Equal(t, agent.AgentTypeClaudeCode, meta.Agent)
}

func TestV2GitStore_WriteCommittedMain_WritesPrompts(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("b2c3d4e5f6a1")
	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-002",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"line":"one"}`)),
		Prompts:      []string{"do the thing", "also this"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// prompt.txt should contain both prompts joined by separator
	promptContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.PromptFileName)
	assert.Contains(t, promptContent, "do the thing")
	assert.Contains(t, promptContent, "also this")

	// raw_transcript_hash.txt should NOT be on /main — it lives on /full/current
	mainSessionTree, err := tree.Tree(cpPath + "/0")
	require.NoError(t, err)
	_, err = mainSessionTree.File(paths.V2RawTranscriptHashFileName)
	assert.Error(t, err, "raw_transcript_hash.txt should not be on /main ref")
}

func TestV2GitStore_WriteCommittedMain_ExcludesTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("c3d4e5f6a1b2")
	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-003",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"line":"one"}` + "\n" + `{"line":"two"}`)),
		Prompts:      []string{"hello"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// raw_transcript should NOT be in the /main tree
	cpTree, err := tree.Tree(cpPath)
	require.NoError(t, err)

	sessionTree, err := cpTree.Tree("0")
	require.NoError(t, err)

	for _, entry := range sessionTree.Entries {
		assert.NotEqual(t, paths.V2RawTranscriptFileName, entry.Name,
			"raw transcript (raw_transcript) must not be on /main ref")
		assert.False(t, strings.HasPrefix(entry.Name, paths.V2RawTranscriptFileName+"."),
			"transcript chunks must not be on /main ref")
	}
}

func TestV2GitStore_WriteCommittedMain_WritesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	compactData := []byte(`{"v":1,"agent":"claude-code","cli_version":"0.1.0","type":"user","ts":"2026-01-01T00:00:00Z","content":"hello"}`)

	cpID := id.MustCheckpointID("d4e5f6a1b2c3")
	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "test-session-compact",
		Strategy:          "manual-commit",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"human","message":"hello"}`)),
		CompactTranscript: compactData,
		Prompts:           []string{"hello"},
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// transcript.jsonl should exist on /main
	transcriptContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.CompactTranscriptFileName)
	assert.Equal(t, string(compactData), transcriptContent)

	// transcript_hash.txt should exist on /main
	hashContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.CompactTranscriptHashFileName)
	assert.True(t, strings.HasPrefix(hashContent, "sha256:"),
		"transcript_hash.txt should be a sha256 hash")

	// SessionFilePaths should repurpose transcript/content_hash for compact artifacts
	summaryContent := v2ReadFile(t, tree, cpPath+"/"+paths.MetadataFileName)
	var summary CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	require.Len(t, summary.Sessions, 1)
	assert.Contains(t, summary.Sessions[0].Transcript, paths.CompactTranscriptFileName)
	assert.Contains(t, summary.Sessions[0].ContentHash, paths.CompactTranscriptHashFileName)
}

func TestV2GitStore_WriteCommittedMain_NoCompactTranscript_SkipsGracefully(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("e5f6a1b2c3d4")
	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "test-session-no-compact",
		Strategy:          "manual-commit",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"human","message":"hello"}`)),
		CompactTranscript: nil,
		Prompts:           []string{"hello"},
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// metadata.json and prompt.txt should still exist
	_ = v2ReadFile(t, tree, cpPath+"/0/"+paths.MetadataFileName)
	_ = v2ReadFile(t, tree, cpPath+"/0/"+paths.PromptFileName)

	// transcript.jsonl should NOT exist
	sessionTree, err := tree.Tree(cpPath + "/0")
	require.NoError(t, err)
	_, err = sessionTree.File(paths.CompactTranscriptFileName)
	assert.Error(t, err, "transcript.jsonl should not exist when CompactTranscript is nil")
}

func TestV2GitStore_WriteCommittedMain_UsesCompactTranscriptStart(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("a1b2c3d4e5f7")
	compactData := []byte("{\"v\":1,\"type\":\"user\",\"content\":\"hello\"}\n{\"v\":1,\"type\":\"assistant\",\"content\":\"hi\"}\n")

	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "test-session-compact-start",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted([]byte(`{"type":"human","message":"hello"}`)),
		CompactTranscript:         compactData,
		Prompts:                   []string{"hello"},
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
		CheckpointTranscriptStart: 42, // raw_transcript offset (must not be used in v2 metadata)
		CompactTranscriptStart:    15, // transcript.jsonl offset (must be used in v2 metadata)
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// Read session metadata from /main
	metadataContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.MetadataFileName)
	var metadata CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &metadata))

	// v2 should store the compact offset, not the full transcript offset.
	assert.Equal(t, 15, metadata.CheckpointTranscriptStart,
		"v2 /main metadata should use CompactTranscriptStart for checkpoint_transcript_start")
}

func TestV2GitStore_UpdateCommitted_WritesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("f6a1b2c3d4e5")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-update-compact",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"human","message":"hello"}`)),
		Prompts:      []string{"hello"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	compactData := []byte(`{"v":1,"agent":"claude-code","cli_version":"0.1.0","type":"user","content":"hello"}`)
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "test-session-update-compact",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"human","message":"hello updated"}`)),
		CompactTranscript: compactData,
		Agent:             "Claude Code",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	transcriptContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.CompactTranscriptFileName)
	assert.Equal(t, string(compactData), transcriptContent)

	hashContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.CompactTranscriptHashFileName)
	assert.True(t, strings.HasPrefix(hashContent, "sha256:"))

	// Root summary paths should stay in sync after UpdateCommitted
	summaryContent := v2ReadFile(t, tree, cpPath+"/"+paths.MetadataFileName)
	var summary CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	require.Len(t, summary.Sessions, 1)
	assert.Contains(t, summary.Sessions[0].Transcript, paths.CompactTranscriptFileName)
	assert.Contains(t, summary.Sessions[0].ContentHash, paths.CompactTranscriptHashFileName)
}

func TestV2GitStore_WriteCommittedMain_MultiSession(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("e5f6a1b2c3d4")

	// First session
	_, err := store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        "session-A",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"line":"a"}`)),
		CheckpointsCount: 3,
		AuthorName:       "Test",
		AuthorEmail:      "test@test.com",
	})
	require.NoError(t, err)

	// Second session (different session ID, same checkpoint)
	_, err = store.writeCommittedMain(ctx, WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        "session-B",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"line":"b"}`)),
		CheckpointsCount: 2,
		AuthorName:       "Test",
		AuthorEmail:      "test@test.com",
	})
	require.NoError(t, err)

	tree := v2MainTree(t, repo)
	cpPath := cpID.Path()

	// Root summary should list 2 sessions
	summaryContent := v2ReadFile(t, tree, cpPath+"/"+paths.MetadataFileName)
	var summary CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	assert.Len(t, summary.Sessions, 2)
	assert.Equal(t, 5, summary.CheckpointsCount, "aggregated count: 3+2")

	// Both session subdirectories should exist
	_ = v2ReadFile(t, tree, cpPath+"/0/"+paths.MetadataFileName)
	_ = v2ReadFile(t, tree, cpPath+"/1/"+paths.MetadataFileName)
}

// v2FullTree returns the root tree from the /full/current ref for test assertions.
func v2FullTree(t *testing.T, repo *git.Repository) *object.Tree {
	t.Helper()
	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	return tree
}

func TestV2GitStore_WriteCommittedFull_WritesTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("f1a2b3c4d5e6")
	transcript := []byte(`{"type":"human","message":"hello"}` + "\n" + `{"type":"assistant","message":"hi"}`)

	err := store.writeCommittedFullTranscript(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-full-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}, 0)
	require.NoError(t, err)

	tree := v2FullTree(t, repo)
	cpPath := cpID.Path()

	// Transcript should exist at session subdirectory 0/
	content := v2ReadFile(t, tree, cpPath+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, content, `"type":"human"`)
	assert.Contains(t, content, `"type":"assistant"`)
}

func TestV2GitStore_WriteCommittedFull_ExcludesMetadata(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("a2b3c4d5e6f1")
	err := store.writeCommittedFullTranscript(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-full-002",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"line":"one"}`)),
		Prompts:      []string{"hello"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}, 0)
	require.NoError(t, err)

	tree := v2FullTree(t, repo)
	cpPath := cpID.Path()

	cpTree, err := tree.Tree(cpPath)
	require.NoError(t, err)

	sessionTree, err := cpTree.Tree("0")
	require.NoError(t, err)

	for _, entry := range sessionTree.Entries {
		assert.NotEqual(t, paths.MetadataFileName, entry.Name,
			"metadata.json must not be on /full/current ref")
		assert.NotEqual(t, paths.PromptFileName, entry.Name,
			"prompt.txt must not be on /full/current ref")
	}

	// raw_transcript_hash.txt SHOULD be on /full/current (co-located with the transcript it hashes)
	hashContent := v2ReadFile(t, tree, cpPath+"/0/"+paths.V2RawTranscriptHashFileName)
	assert.True(t, strings.HasPrefix(hashContent, "sha256:"), "content hash should be sha256 prefixed")
}

func TestV2GitStore_WriteCommittedFull_NoTranscript_Noop(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("b3c4d5e6f1a2")
	err := store.writeCommittedFullTranscript(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-full-003",
		Strategy:     "manual-commit",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}, 0)
	require.NoError(t, err)

	// /full/current ref should either not exist or have an empty tree
	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	if err == nil {
		commit, cErr := repo.CommitObject(ref.Hash())
		require.NoError(t, cErr)
		tree, tErr := commit.Tree()
		require.NoError(t, tErr)
		assert.Empty(t, tree.Entries, "empty transcript should produce no entries")
	}
	// If ref doesn't exist at all, that's also acceptable for a no-op
}

func TestV2GitStore_WriteCommittedFullTranscript_AccumulatesCheckpoints(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpA := id.MustCheckpointID("c4d5e6f1a2b3")
	cpB := id.MustCheckpointID("d5e6f1a2b3c4")

	// Write checkpoint A
	err := store.writeCommittedFullTranscript(ctx, WriteCommittedOptions{
		CheckpointID: cpA,
		SessionID:    "session-A",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"from":"A"}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}, 0)
	require.NoError(t, err)

	// Write checkpoint B — should accumulate alongside A
	err = store.writeCommittedFullTranscript(ctx, WriteCommittedOptions{
		CheckpointID: cpB,
		SessionID:    "session-B",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"from":"B"}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}, 0)
	require.NoError(t, err)

	tree := v2FullTree(t, repo)

	// Both checkpoints should be present
	contentA := v2ReadFile(t, tree, cpA.Path()+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, contentA, `"from":"A"`)

	contentB := v2ReadFile(t, tree, cpB.Path()+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, contentB, `"from":"B"`)
}

func TestV2GitStore_WriteCommitted_WritesBothRefs(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("aa11bb22cc33")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-both",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"hello"}`)),
		Prompts:      []string{"hi there"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	cpPath := cpID.Path()

	// /main ref should have metadata and prompt — no transcript or content hash
	mainTree := v2MainTree(t, repo)
	_ = v2ReadFile(t, mainTree, cpPath+"/"+paths.MetadataFileName)
	_ = v2ReadFile(t, mainTree, cpPath+"/0/"+paths.MetadataFileName)
	_ = v2ReadFile(t, mainTree, cpPath+"/0/"+paths.PromptFileName)

	mainSessionTree, err := mainTree.Tree(cpPath + "/0")
	require.NoError(t, err)
	for _, entry := range mainSessionTree.Entries {
		assert.NotEqual(t, paths.V2RawTranscriptFileName, entry.Name)
		assert.NotEqual(t, paths.V2RawTranscriptHashFileName, entry.Name)
	}

	// /full/current ref should have transcript + content hash
	fullTree := v2FullTree(t, repo)
	content := v2ReadFile(t, fullTree, cpPath+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, content, `"type":"assistant"`)
	hashContent := v2ReadFile(t, fullTree, cpPath+"/0/"+paths.V2RawTranscriptHashFileName)
	assert.True(t, strings.HasPrefix(hashContent, "sha256:"))
}

func TestV2GitStore_WriteCommitted_NoTranscript_OnlyWritesMain(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("bb22cc33dd44")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-notx",
		Strategy:     "manual-commit",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// /main should have metadata
	mainTree := v2MainTree(t, repo)
	_ = v2ReadFile(t, mainTree, cpID.Path()+"/0/"+paths.MetadataFileName)

	// /full/current ref should not exist (no transcript = no-op for full)
	_, err = repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	assert.Error(t, err, "/full/current should not exist when no transcript is written")
}

func TestV2GitStore_WriteCommitted_MultiSession_ConsistentIndex(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("cc33dd44ee55")

	// First session
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        "session-X",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"from":"X"}`)),
		CheckpointsCount: 2,
		AuthorName:       "Test",
		AuthorEmail:      "test@test.com",
	})
	require.NoError(t, err)

	// Second session — same checkpoint, different session ID
	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        "session-Y",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"from":"Y"}`)),
		CheckpointsCount: 3,
		AuthorName:       "Test",
		AuthorEmail:      "test@test.com",
	})
	require.NoError(t, err)

	cpPath := cpID.Path()

	// /main should have both sessions
	mainTree := v2MainTree(t, repo)
	summaryContent := v2ReadFile(t, mainTree, cpPath+"/"+paths.MetadataFileName)
	var summary CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	assert.Len(t, summary.Sessions, 2)

	// /full/current should have session Y (latest write replaces)
	fullTree := v2FullTree(t, repo)
	contentY := v2ReadFile(t, fullTree, cpPath+"/1/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, contentY, `"from":"Y"`)
}

func TestV2GitStore_UpdateCommitted_UpdatesBothRefs(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("ff11aa22bb33")

	// Initial write
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-update",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"initial"}`)),
		Prompts:      []string{"first prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Update with finalized transcript and prompts
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-update",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"finalized"}`)),
		Prompts:      []string{"first prompt", "second prompt"},
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	cpPath := cpID.Path()

	// /main should have updated prompts
	mainTree := v2MainTree(t, repo)
	promptContent := v2ReadFile(t, mainTree, cpPath+"/0/"+paths.PromptFileName)
	assert.Contains(t, promptContent, "second prompt")

	// /full/current should have finalized transcript
	fullTree := v2FullTree(t, repo)
	content := v2ReadFile(t, fullTree, cpPath+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, content, "finalized")
	assert.NotContains(t, content, "initial")
}

func TestV2GitStore_UpdateCommitted_NoTranscript_OnlyUpdatesMain(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("aa33bb44cc55")

	// Initial write with transcript
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-noupdate",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"original"}`)),
		Prompts:      []string{"old prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Update with only prompts (no transcript)
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-noupdate",
		Prompts:      []string{"old prompt", "new prompt"},
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	// /main should have updated prompts
	mainTree := v2MainTree(t, repo)
	promptContent := v2ReadFile(t, mainTree, cpID.Path()+"/0/"+paths.PromptFileName)
	assert.Contains(t, promptContent, "new prompt")

	// /full/current should still have original transcript (not replaced)
	fullTree := v2FullTree(t, repo)
	content := v2ReadFile(t, fullTree, cpID.Path()+"/0/"+paths.V2RawTranscriptFileName)
	assert.Contains(t, content, "original")
}

func TestV2GitStore_UpdateCommitted_CheckpointNotFound(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("bb44cc55dd66")

	// Update without prior write should return error
	err := store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "nonexistent",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"hello"}`)),
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.Error(t, err)
}

func TestV2GitStore_UpdateCommitted_PreservesExistingTaskMetadataInFullCurrent(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpID := id.MustCheckpointID("cc55dd66ee77")

	// Initial write creates checkpoint/session on both /main and /full/current.
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-task-preserve",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"initial"}`)),
		Prompts:      []string{"first prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Inject task metadata into /full/current to emulate condensation-time task copy.
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := store.GetRefState(refName)
	require.NoError(t, err)

	taskPath := []string{string(cpID[:2]), string(cpID[2:]), "0", "tasks", "toolu_01TASK"}
	checkpointJSON := []byte(`{"session_id":"test-session-task-preserve","tool_use_id":"toolu_01TASK"}`)
	blobHash, err := CreateBlobFromContent(repo, checkpointJSON)
	require.NoError(t, err)

	newRootHash, err := UpdateSubtree(repo, rootTreeHash,
		taskPath,
		[]object.TreeEntry{{Name: "checkpoint.json", Mode: filemode.Regular, Hash: blobHash}},
		UpdateSubtreeOptions{MergeMode: MergeKeepExisting},
	)
	require.NoError(t, err)

	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	commitHash, err := CreateCommit(ctx, repo, newRootHash, parentHash,
		fmt.Sprintf("Checkpoint: %s (task metadata)\n", cpID), authorName, authorEmail)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))

	// Finalize checkpoint with full transcript (the stop-time path).
	err = store.UpdateCommitted(ctx, UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-task-preserve",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"finalized"}`)),
		Prompts:      []string{"first prompt", "second prompt"},
		Agent:        agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	// Task metadata should still exist after UpdateCommitted.
	fullTree := v2FullTree(t, repo)
	_, err = fullTree.File(cpID.Path() + "/0/tasks/toolu_01TASK/checkpoint.json")
	require.NoError(t, err, "task metadata should be preserved on /full/current during UpdateCommitted")
}

func TestWriteCommitted_TriggersRotationAtThreshold(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 3 // Low threshold for testing
	ctx := context.Background()

	// Write 3 checkpoints — the 3rd should trigger rotation
	for i := range 3 {
		cpID := id.MustCheckpointID(fmt.Sprintf("%012x", i+1))
		err := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    fmt.Sprintf("session-rot-%d", i),
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   redact.AlreadyRedacted([]byte(fmt.Sprintf(`{"cp":%d}`, i))),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}

	// Verify an archived generation exists
	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Len(t, archived, 1, "one archived generation should exist after rotation")

	// Verify /full/current is now a fresh generation (empty tree, no generation.json)
	_, freshTreeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	freshCount, err := store.CountCheckpointsInTree(freshTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 0, freshCount, "fresh /full/current should have no checkpoints")

	// Verify the archived generation has 3 checkpoints
	_, archiveTreeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2FullRefPrefix + archived[0]))
	require.NoError(t, err)
	archiveCount, err := store.CountCheckpointsInTree(archiveTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 3, archiveCount)

	// Write a 4th checkpoint — should land on the fresh /full/current
	cpID4 := id.MustCheckpointID("000000000004")
	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID4,
		SessionID:    "session-rot-3",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   redact.AlreadyRedacted([]byte(`{"cp":3}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	_, newTreeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	newCount, err := store.CountCheckpointsInTree(newTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 1, newCount, "new checkpoint should be on fresh generation")
}

func TestWriteCommitted_NoRotationBelowThreshold(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 5
	ctx := context.Background()

	// Write 3 checkpoints (below threshold of 5)
	for i := range 3 {
		cpID := id.MustCheckpointID(fmt.Sprintf("%012x", i+100))
		err := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    fmt.Sprintf("session-norot-%d", i),
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   redact.AlreadyRedacted([]byte(fmt.Sprintf(`{"cp":%d}`, i))),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		})
		require.NoError(t, err)
	}

	// No rotation should have occurred
	archived, err := store.ListArchivedGenerations()
	require.NoError(t, err)
	assert.Empty(t, archived, "no archived generations should exist below threshold")

	_, noRotTreeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)
	noRotCount, err := store.CountCheckpointsInTree(noRotTreeHash)
	require.NoError(t, err)
	assert.Equal(t, 3, noRotCount)
}

// The index must agree with the per-call HasFullSessionArtifacts predicate
// for every (checkpoint, session) pair — the migration loop relies on them
// being interchangeable.
func TestV2GitStore_BuildFullSessionArtifactsIndex_AgreesWithHasFullSessionArtifacts(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	cpIDs := []id.CheckpointID{
		id.MustCheckpointID("aa11bb22cc33"),
		id.MustCheckpointID("dd44ee55ff66"),
		id.MustCheckpointID("11223344aabb"),
	}
	for i, cpID := range cpIDs {
		require.NoError(t, store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    fmt.Sprintf("session-index-%d", i),
			Strategy:     "manual-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   redact.AlreadyRedacted([]byte(fmt.Sprintf(`{"cp":%d}`, i))),
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
		}))
	}

	index, err := store.BuildFullSessionArtifactsIndex()
	require.NoError(t, err)

	for _, cpID := range cpIDs {
		// All three writes hit session 0; sessions ≥1 must be absent from
		// both predicate and index.
		for sessionIdx := range 3 {
			fromCall, err := store.HasFullSessionArtifacts(cpID, sessionIdx)
			require.NoError(t, err)
			fromIndex := index.Has(cpID, sessionIdx)
			assert.Equal(t, fromCall, fromIndex,
				"index disagreement for %s/%d (call=%v, index=%v)", cpID, sessionIdx, fromCall, fromIndex)
		}
	}

	// Unknown checkpoints must not be in the index.
	missing := id.MustCheckpointID("999999999999")
	assert.False(t, index.Has(missing, 0))
}

// A nil index — the documented test-only fallback — must not panic on Has.
func TestV2GitStore_BuildFullSessionArtifactsIndex_NilSafe(t *testing.T) {
	t.Parallel()
	var index FullSessionArtifactsIndex
	assert.False(t, index.Has(id.MustCheckpointID("abc123def456"), 0))
}
