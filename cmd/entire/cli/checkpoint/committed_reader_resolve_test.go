package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestCommittedReader_UsesV2WhenFound(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("111111111111")

	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	summary, err := ReadCommittedCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)

	content, err := reader.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.Equal(t, "session-v2", content.Metadata.SessionID)
}

func TestNewCommittedReader_SelectsMode(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadV1)
	require.NoError(t, err)
	require.IsType(t, &GitStore{}, reader)

	reader, err = NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	require.IsType(t, &DualCheckpointReader{}, reader)

	reader, err = NewCommittedReader(v1Store, v2Store, CommittedReadV2)
	require.NoError(t, err)
	require.IsType(t, &V2GitStore{}, reader)
}

func TestCommittedReadModeForOptions(t *testing.T) {
	t.Parallel()

	require.Equal(t, CommittedReadV1, CommittedReadModeForOptions(false, 1))
	require.Equal(t, CommittedReadDual, CommittedReadModeForOptions(true, 1))
	require.Equal(t, CommittedReadV2, CommittedReadModeForOptions(true, 2))
	require.Equal(t, CommittedReadV2, CommittedReadModeForOptions(false, 2))
}

func TestDualCheckpointReader_FallsBackToV1RawTranscriptBySessionID(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("121212121212")

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-a",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1-session-a"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-b",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1-session-b"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "session-b",
		Strategy:          "manual-commit",
		CompactTranscript: []byte(`{"text":"compact-session-b"}` + "\n"),
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	summary, err := ReadCommittedCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.Len(t, summary.Sessions, 1)

	content, err := reader.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.Equal(t, "session-b", content.Metadata.SessionID)
	require.Contains(t, string(content.Transcript), "from-v1-session-b")
	require.NotContains(t, string(content.Transcript), "from-v1-session-a")
}

func TestDualCheckpointReader_DoesNotUseIndexFallbackWhenV2CheckpointExists(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("787878787878")

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-a",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1-session-a"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-b",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1-session-b"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	v1IndexZero, err := v1Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.Equal(t, "session-a", v1IndexZero.Metadata.SessionID)

	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "session-b",
		Strategy:          "manual-commit",
		CompactTranscript: []byte(`{"text":"compact-session-b"}` + "\n"),
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	}))
	removeV2MainSessionTree(t, repo, cpID)

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)

	content, err := reader.ReadSessionContent(ctx, cpID, 0)
	require.Nil(t, content)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	metadata, err := reader.ReadSessionMetadata(ctx, cpID, 0)
	require.Nil(t, metadata)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

func TestDualCheckpointReader_ReadSessionContentReturnsV2AndFallbackErrors(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("565656565656")

	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "session-missing-v1",
		Strategy:          "manual-commit",
		CompactTranscript: []byte(`{"text":"compact-only"}` + "\n"),
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)

	content, err := reader.ReadSessionContent(ctx, cpID, 0)
	require.Nil(t, content)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoTranscript)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
	require.Contains(t, err.Error(), "read v1 fallback session content")
}

func TestReadRawSessionLogForCheckpoint_FallsBackToV1RawTranscriptByV2SessionID(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("343434343434")

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-b",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1-session-b"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-a",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1-session-a"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "session-b",
		Strategy:          "manual-commit",
		CompactTranscript: []byte(`{"text":"compact-session-b"}` + "\n"),
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	logContent, sessionID, err := ReadRawSessionLogForCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.Equal(t, "session-b", sessionID)
	require.Contains(t, string(logContent), "from-v1-session-b")
	require.NotContains(t, string(logContent), "from-v1-session-a")
}

func TestCommittedReader_FallsBackToV1WhenMissingInV2(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("222222222222")

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	summary, err := ReadCommittedCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)

	content, err := reader.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.Equal(t, "session-v1", content.Metadata.SessionID)
}

func TestCommittedReader_PrefersV1WhenV2Disabled(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("333333333333")

	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadV1)
	require.NoError(t, err)
	summary, err := ReadCommittedCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.IsType(t, &GitStore{}, reader)
}

func TestReadRawSessionLogForCheckpoint_UsesV2WhenFound(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("444444444444")

	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-v2"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	logContent, sessionID, err := ReadRawSessionLogForCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.Equal(t, "session-v2", sessionID)
	require.Contains(t, string(logContent), "from-v2")
}

func TestDualCheckpointReader_ListCommittedMergesV2AndV1(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	transcript := redact.AlreadyRedacted([]byte(`{"text":"hello"}` + "\n"))

	v1OnlyID := id.MustCheckpointID("888888888888")
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: v1OnlyID,
		SessionID:    "session-v1-only",
		Strategy:     "manual-commit",
		Transcript:   transcript,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	dualID := id.MustCheckpointID("999999999999")
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: dualID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   transcript,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: dualID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   transcript,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)

	results, err := reader.ListCommitted(ctx)
	require.NoError(t, err)

	counts := map[id.CheckpointID]int{}
	for _, result := range results {
		counts[result.CheckpointID]++
	}
	require.Equal(t, 1, counts[v1OnlyID])
	require.Equal(t, 1, counts[dualID])
}

func TestCommittedReadV2DoesNotFallBackToV1(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("abababababab")

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-v1"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadV2)
	require.NoError(t, err)

	summary, err := reader.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.Nil(t, summary)
}

func TestReadRawSessionLogForCheckpoint_FallsBackToV1WhenMissingInV2(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("555555555555")

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-v1"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	logContent, sessionID, err := ReadRawSessionLogForCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.Equal(t, "session-v1", sessionID)
	require.Contains(t, string(logContent), "from-v1")
}

func TestReadRawSessionLogForCheckpoint_PrefersV1WhenV2Disabled(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("666666666666")

	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-v2"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-v1"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadV1)
	require.NoError(t, err)
	logContent, sessionID, err := ReadRawSessionLogForCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.Equal(t, "session-v1", sessionID)
	require.Contains(t, string(logContent), "from-v1")
}

func TestCommittedReader_DoesNotUseIndexFallbackWhenV2Malformed(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	v1Store := NewGitStore(repo)
	v2Store := NewV2GitStore(repo, "origin")
	ctx := context.Background()
	cpID := id.MustCheckpointID("777777777777")

	// Write valid v1 checkpoint.
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-v1"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, v1Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-other",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-other"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	// Write valid v2 checkpoint, then corrupt its metadata.json.
	require.NoError(t, v2Store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"from-v2"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	corruptV2MainMetadata(t, repo, cpID)

	reader, err := NewCommittedReader(v1Store, v2Store, CommittedReadDual)
	require.NoError(t, err)
	summary, err := ReadCommittedCheckpoint(ctx, reader, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	content, err := reader.ReadSessionContent(ctx, cpID, 0)
	require.Nil(t, content)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

// corruptV2MainMetadata replaces the v2 /main ref tree with one containing
// invalid JSON in the checkpoint's metadata.json, causing ReadCommitted to
// return a parse error (not a sentinel error).
func corruptV2MainMetadata(t *testing.T, repo *git.Repository, cpID id.CheckpointID) {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	ref, err := repo.Storer.Reference(refName)
	require.NoError(t, err)
	parentHash := ref.Hash()

	garbageBlob, err := CreateBlobFromContent(repo, []byte(`{invalid json`))
	require.NoError(t, err)

	// cpID.Path() returns "ab/cdef123456" — split into shard dir and remainder.
	parts := strings.SplitN(cpID.Path(), "/", 2)
	require.Len(t, parts, 2)

	cpTreeHash, err := storeTree(repo, []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: garbageBlob},
	})
	require.NoError(t, err)

	shardTreeHash, err := storeTree(repo, []object.TreeEntry{
		{Name: parts[1], Mode: filemode.Dir, Hash: cpTreeHash},
	})
	require.NoError(t, err)

	rootTreeHash, err := storeTree(repo, []object.TreeEntry{
		{Name: parts[0], Mode: filemode.Dir, Hash: shardTreeHash},
	})
	require.NoError(t, err)

	commitHash, err := CreateCommit(context.Background(), repo, rootTreeHash, parentHash,
		"corrupt metadata for test", "Test", "test@test.com")
	require.NoError(t, err)

	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(refName, commitHash)))
}

func removeV2MainSessionTree(t *testing.T, repo *git.Repository, cpID id.CheckpointID) {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	ref, err := repo.Storer.Reference(refName)
	require.NoError(t, err)
	parentHash := ref.Hash()

	parentCommit, err := repo.CommitObject(parentHash)
	require.NoError(t, err)
	rootTree, err := parentCommit.Tree()
	require.NoError(t, err)

	cpTree, err := rootTree.Tree(cpID.Path())
	require.NoError(t, err)
	metadataFile, err := cpTree.File(paths.MetadataFileName)
	require.NoError(t, err)

	parts := strings.SplitN(cpID.Path(), "/", 2)
	require.Len(t, parts, 2)

	cpTreeHash, err := storeTree(repo, []object.TreeEntry{
		{Name: paths.MetadataFileName, Mode: filemode.Regular, Hash: metadataFile.Hash},
	})
	require.NoError(t, err)

	shardTreeHash, err := storeTree(repo, []object.TreeEntry{
		{Name: parts[1], Mode: filemode.Dir, Hash: cpTreeHash},
	})
	require.NoError(t, err)

	rootTreeHash, err := storeTree(repo, []object.TreeEntry{
		{Name: parts[0], Mode: filemode.Dir, Hash: shardTreeHash},
	})
	require.NoError(t, err)

	commitHash, err := CreateCommit(context.Background(), repo, rootTreeHash, parentHash,
		"remove v2 session tree for test", "Test", "test@test.com")
	require.NoError(t, err)

	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(refName, commitHash)))
}
