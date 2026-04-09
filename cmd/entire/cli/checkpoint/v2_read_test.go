package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestV2ReadCommitted_ReturnsCheckpointSummary(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("a1a2a3a4a5a6")
	ctx := context.Background()

	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"test": true}`),
		Prompts:      []string{"hello"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	summary, err := store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, cpID, summary.CheckpointID)
	assert.Len(t, summary.Sessions, 1)
}

func TestV2ReadCommitted_ReturnsNilForMissing(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("b1b2b3b4b5b6")
	ctx := context.Background()

	summary, err := store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	assert.Nil(t, summary)
}

func TestV2ReadSessionContent_ReturnsMetadataAndTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("c1c2c3c4c5c6")
	ctx := context.Background()

	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"message": "hello world"}`),
		Prompts:      []string{"test prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	content, err := store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.Equal(t, "session-1", content.Metadata.SessionID)
	assert.NotEmpty(t, content.Transcript)
	assert.Contains(t, content.Prompts, "test prompt")
}

func TestV2ReadSessionContent_TranscriptFromArchivedGeneration(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	store.maxCheckpointsPerGeneration = 1
	ctx := context.Background()

	cpID1 := id.MustCheckpointID("d1d2d3d4d5d6")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID1,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"first": true}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	cpID2 := id.MustCheckpointID("e1e2e3e4e5e6")
	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID2,
		SessionID:    "session-2",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"second": true}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	content, err := store.ReadSessionContent(ctx, cpID1, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.NotEmpty(t, content.Transcript, "transcript should be found in archived generation")
}

func TestV2ReadSessionContent_MissingTranscript_ReturnsError(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("f1f2f3f4f5f6")
	ctx := context.Background()

	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Prompts:      []string{"prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	_, err = store.ReadSessionContent(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrNoTranscript)
}

func TestV2ReadSessionContent_ChunkedTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	cpID := id.MustCheckpointID("a0a1a2a3a4a5")
	ctx := context.Background()

	// Write metadata to /main so ReadSessionContent can find the checkpoint
	v2Store := NewV2GitStore(repo, "origin")
	err := v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-chunked",
		Strategy:     "manual-commit",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Manually write chunked transcript to /full/current:
	// chunk 0 = full.jsonl (base file), chunk 1 = full.jsonl.001
	chunk0 := []byte(`{"line":"one"}` + "\n" + `{"line":"two"}`)
	chunk1 := []byte(`{"line":"three"}` + "\n" + `{"line":"four"}`)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	err = v2Store.ensureRef(refName)
	require.NoError(t, err)

	_, rootTreeHash, err := v2Store.GetRefState(refName)
	require.NoError(t, err)

	sessionPath := cpID.Path() + "/0/"

	// Create blobs for each chunk
	blob0, err := CreateBlobFromContent(repo, chunk0)
	require.NoError(t, err)
	blob1, err := CreateBlobFromContent(repo, chunk1)
	require.NoError(t, err)

	entries := map[string]object.TreeEntry{
		sessionPath + paths.TranscriptFileName: {
			Name: sessionPath + paths.TranscriptFileName,
			Mode: filemode.Regular,
			Hash: blob0,
		},
		sessionPath + paths.TranscriptFileName + ".001": {
			Name: sessionPath + paths.TranscriptFileName + ".001",
			Mode: filemode.Regular,
			Hash: blob1,
		},
	}

	newTreeHash, err := v2Store.gs.spliceCheckpointSubtree(rootTreeHash, cpID, cpID.Path()+"/", entries)
	require.NoError(t, err)

	parentHash, _, err := v2Store.GetRefState(refName)
	require.NoError(t, err)
	err = v2Store.updateRef(refName, newTreeHash, parentHash, "chunked test", "Test", "test@test.com")
	require.NoError(t, err)

	// Read it back — should reassemble both chunks
	content, err := v2Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)

	transcript := string(content.Transcript)
	assert.Contains(t, transcript, `{"line":"one"}`)
	assert.Contains(t, transcript, `{"line":"two"}`)
	assert.Contains(t, transcript, `{"line":"three"}`)
	assert.Contains(t, transcript, `{"line":"four"}`)
}
