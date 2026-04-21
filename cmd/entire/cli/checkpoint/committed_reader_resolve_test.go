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

func TestResolveCommittedReaderForCheckpoint_UsesV2WhenFound(t *testing.T) {
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

	reader, summary, err := ResolveCommittedReaderForCheckpoint(ctx, cpID, v1Store, v2Store, true)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.IsType(t, &V2GitStore{}, reader)
}

func TestResolveCommittedReaderForCheckpoint_FallsBackToV1WhenMissingInV2(t *testing.T) {
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

	reader, summary, err := ResolveCommittedReaderForCheckpoint(ctx, cpID, v1Store, v2Store, true)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.IsType(t, &GitStore{}, reader)
}

func TestResolveCommittedReaderForCheckpoint_PrefersV1WhenV2Disabled(t *testing.T) {
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

	reader, summary, err := ResolveCommittedReaderForCheckpoint(ctx, cpID, v1Store, v2Store, false)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.IsType(t, &GitStore{}, reader)
}

func TestResolveRawSessionLogForCheckpoint_UsesV2WhenFound(t *testing.T) {
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

	logContent, sessionID, err := ResolveRawSessionLogForCheckpoint(ctx, cpID, v1Store, v2Store, true)
	require.NoError(t, err)
	require.Equal(t, "session-v2", sessionID)
	require.Contains(t, string(logContent), "from-v2")
}

func TestResolveRawSessionLogForCheckpoint_FallsBackToV1WhenMissingInV2(t *testing.T) {
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

	logContent, sessionID, err := ResolveRawSessionLogForCheckpoint(ctx, cpID, v1Store, v2Store, true)
	require.NoError(t, err)
	require.Equal(t, "session-v1", sessionID)
	require.Contains(t, string(logContent), "from-v1")
}

func TestResolveRawSessionLogForCheckpoint_PrefersV1WhenV2Disabled(t *testing.T) {
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

	logContent, sessionID, err := ResolveRawSessionLogForCheckpoint(ctx, cpID, v1Store, v2Store, false)
	require.NoError(t, err)
	require.Equal(t, "session-v1", sessionID)
	require.Contains(t, string(logContent), "from-v1")
}

func TestResolveCommittedReaderForCheckpoint_FallsBackToV1WhenV2Malformed(t *testing.T) {
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

	// Should fall back to v1 instead of propagating the v2 parse error.
	reader, summary, err := ResolveCommittedReaderForCheckpoint(ctx, cpID, v1Store, v2Store, true)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.IsType(t, &GitStore{}, reader)
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
