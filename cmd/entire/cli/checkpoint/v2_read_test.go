package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
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
		Transcript:   redact.AlreadyRedacted([]byte(`{"test": true}`)),
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
		Transcript:   redact.AlreadyRedacted([]byte(`{"message": "hello world"}`)),
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
		Transcript:   redact.AlreadyRedacted([]byte(`{"first": true}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	cpID2 := id.MustCheckpointID("e1e2e3e4e5e6")
	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID2,
		SessionID:    "session-2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"second": true}`)),
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

func TestV2ReadSessionMetadataAndPrompts_ReturnsWithoutTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("f1f2f3f4f5f7")
	ctx := context.Background()

	// Write a checkpoint with prompts but no transcript (WriteCommitted skips
	// /full/current when Transcript is empty).
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-meta-only",
		Strategy:     "manual-commit",
		Prompts:      []string{"test prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// ReadSessionContent should fail (no transcript).
	_, err = store.ReadSessionContent(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrNoTranscript)

	// ReadSessionMetadataAndPrompts should succeed.
	content, err := store.ReadSessionMetadataAndPrompts(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.Equal(t, "session-meta-only", content.Metadata.SessionID)
	assert.Contains(t, content.Prompts, "test prompt")
	assert.Empty(t, content.Transcript)
}

func TestV2ReadSessionMetadataAndPrompts_MissingCheckpoint(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("f1f2f3f4f5f8")
	ctx := context.Background()

	_, err := store.ReadSessionMetadataAndPrompts(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
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
	// chunk 0 = raw_transcript (base file), chunk 1 = raw_transcript.001
	chunk0 := []byte(`{"line":"one"}` + "\n" + `{"line":"two"}`)
	chunk1 := []byte(`{"line":"three"}` + "\n" + `{"line":"four"}`)

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	err = v2Store.ensureRef(context.Background(), refName)
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
		sessionPath + paths.V2RawTranscriptFileName: {
			Name: sessionPath + paths.V2RawTranscriptFileName,
			Mode: filemode.Regular,
			Hash: blob0,
		},
		sessionPath + paths.V2RawTranscriptFileName + ".001": {
			Name: sessionPath + paths.V2RawTranscriptFileName + ".001",
			Mode: filemode.Regular,
			Hash: blob1,
		},
	}

	newTreeHash, err := v2Store.gs.spliceCheckpointSubtree(context.Background(), rootTreeHash, cpID, cpID.Path()+"/", entries)
	require.NoError(t, err)

	parentHash, _, err := v2Store.GetRefState(refName)
	require.NoError(t, err)
	err = v2Store.updateRef(ctx, refName, newTreeHash, parentHash, "chunked test", "Test", "test@test.com")
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

func TestV2ReadSessionCompactTranscript_ReturnsCompactData(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("b0b1b2b3b4b5")
	ctx := context.Background()

	compact := []byte(`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","content":[{"text":"hello compact"}]}` + "\n")
	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "session-compact",
		Strategy:          "manual-commit",
		Transcript:        redact.AlreadyRedacted([]byte(`{"raw":true}` + "\n")),
		CompactTranscript: compact,
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	})
	require.NoError(t, err)

	content, err := store.ReadSessionCompactTranscript(ctx, cpID, 0)
	require.NoError(t, err)
	require.Equal(t, compact, content)
}

func TestV2ReadSessionCompactTranscript_MissingCompactTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("c0c1c2c3c4c5")
	ctx := context.Background()

	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-no-compact",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"raw":true}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	_, err = store.ReadSessionCompactTranscript(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrNoTranscript)
}

func TestV2ReadSessionCompactTranscript_MissingCheckpointOrSession(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	_, err := store.ReadSessionCompactTranscript(ctx, id.MustCheckpointID("d0d1d2d3d4d5"), 0)
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	cpID := id.MustCheckpointID("e0e1e2e3e4e5")
	require.NoError(t, store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-0",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"raw":true}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	_, err = store.ReadSessionCompactTranscript(ctx, cpID, 99)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

func TestV2UpdateSummary_PersistsSummaryToLatestSession(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("f0f1f2f3f4f5")
	ctx := context.Background()

	err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-summary-test",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// No summary initially
	summary, err := store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	content, err := store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.Nil(t, content.Metadata.Summary)

	// Update with a summary
	err = store.UpdateSummary(ctx, cpID, &Summary{
		Intent:  "Test v2 intent",
		Outcome: "Test v2 outcome",
	})
	require.NoError(t, err)

	// Verify summary persisted
	content, err = store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content.Metadata.Summary)
	assert.Equal(t, "Test v2 intent", content.Metadata.Summary.Intent)
	assert.Equal(t, "Test v2 outcome", content.Metadata.Summary.Outcome)

	// Verify other metadata preserved
	assert.Equal(t, "session-summary-test", content.Metadata.SessionID)
	_ = summary // used above
}

func TestV2UpdateSummary_NotFound(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")
	ctx := context.Background()

	err := store.UpdateSummary(ctx, id.MustCheckpointID("000000000000"), &Summary{Intent: "x"})
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}
