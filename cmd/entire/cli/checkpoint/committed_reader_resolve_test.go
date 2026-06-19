package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

func TestReadCommittedCheckpointNormalizesNilSummary(t *testing.T) {
	t.Parallel()

	reader := &committedReaderStub{}
	summary, err := ReadCommittedCheckpoint(context.Background(), reader, id.MustCheckpointID("111111111111"))
	require.Nil(t, summary)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

func TestReadCommittedCheckpointWrapsReaderError(t *testing.T) {
	t.Parallel()

	readerErr := errors.New("boom")
	reader := &committedReaderStub{readErr: readerErr}
	summary, err := ReadCommittedCheckpoint(context.Background(), reader, id.MustCheckpointID("111111111111"))
	require.Nil(t, summary)
	require.ErrorIs(t, err, readerErr)
	require.ErrorContains(t, err, "read committed checkpoint")
}

func TestReadLatestSessionContentEmptySummaryReturnsNotFound(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("111111111111")
	summary := &CheckpointSummary{}
	reader := &committedReaderStub{summary: summary}

	content, err := ReadLatestSessionContent(context.Background(), reader, cpID, summary)
	require.Nil(t, content)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

func TestReadRawSessionLogForCheckpointReadsLatestV1Session(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)

	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("222222222222")

	require.NoError(t, store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-a",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("first transcript\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	require.NoError(t, store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-b",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("latest transcript\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	transcript, sessionID, err := ReadRawSessionLogForCheckpoint(ctx, store, cpID)
	require.NoError(t, err)
	require.Equal(t, "session-b", sessionID)
	require.Equal(t, []byte("latest transcript\n"), transcript)
}

func TestGitStoreSessionStoreReadsByRef(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)

	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("333333333333")

	writeSessionForStoreTest(t, store, cpID, "session-a", "first transcript\n", "first prompt")
	writeSessionForStoreTest(t, store, cpID, "session-b", "latest transcript\n", "latest prompt")

	latest, err := store.ReadSession(ctx, LatestSessionRef(cpID))
	require.NoError(t, err)
	require.Equal(t, "session-b", latest.Metadata.SessionID)
	require.Equal(t, []byte("latest transcript\n"), latest.Transcript)

	metadataOnly, err := store.ReadSession(ctx, SessionIndexRef(cpID, 0), WithSessionMetadataOnly())
	require.NoError(t, err)
	require.Equal(t, "session-a", metadataOnly.Metadata.SessionID)
	require.Empty(t, metadataOnly.Transcript)
	require.Empty(t, metadataOnly.Prompts)

	metadataAndPrompts, err := store.ReadSession(ctx, SessionIDRef(cpID, "session-a"), WithSessionMetadataAndPrompts())
	require.NoError(t, err)
	require.Equal(t, "session-a", metadataAndPrompts.Metadata.SessionID)
	require.Equal(t, "first prompt", metadataAndPrompts.Prompts)
	require.Empty(t, metadataAndPrompts.Transcript)

	infos, err := store.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, cpID, infos[0].CheckpointID)
}

func TestResolveSessionIndexUsesIndexRefDirectly(t *testing.T) {
	t.Parallel()

	store := &GitStore{}
	cpID := id.MustCheckpointID("555555555555")

	sessionIndex, err := store.resolveSessionIndex(context.Background(), SessionIndexRef(cpID, 3))
	require.NoError(t, err)
	require.Equal(t, 3, sessionIndex)
}

func TestGitStoreStoresUpdateSpecificSessionAndCheckpoint(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)

	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("444444444444")

	writeSessionForStoreTest(t, store, cpID, "session-a", "first transcript\n", "first prompt")
	writeSessionForStoreTest(t, store, cpID, "session-b", "latest transcript\n", "latest prompt")

	sessionSummary := &Summary{Intent: "summarize first session"}
	require.NoError(t, store.UpdateSession(ctx, SessionIDRef(cpID, "session-a"), WithSummary(sessionSummary)))

	first, err := store.ReadSession(ctx, SessionIDRef(cpID, "session-a"), WithSessionMetadataOnly())
	require.NoError(t, err)
	require.Equal(t, sessionSummary.Intent, first.Metadata.Summary.Intent)

	second, err := store.ReadSession(ctx, SessionIDRef(cpID, "session-b"), WithSessionMetadataOnly())
	require.NoError(t, err)
	require.Nil(t, second.Metadata.Summary)

	attribution := &InitialAttribution{AgentLines: 7, TotalCommitted: 11}
	require.NoError(t, store.UpdateCheckpoint(ctx, cpID, WithAttribution(attribution)))

	checkpointSummary, err := store.ReadCheckpoint(ctx, cpID)
	require.NoError(t, err)
	require.Equal(t, 7, checkpointSummary.CombinedAttribution.AgentLines)
	require.Equal(t, 11, checkpointSummary.CombinedAttribution.TotalCommitted)
}

func writeSessionForStoreTest(t *testing.T, store *GitStore, cpID id.CheckpointID, sessionID, transcript, prompt string) {
	t.Helper()

	require.NoError(t, store.WriteSession(t.Context(), SessionIDRef(cpID, sessionID), Session{
		Strategy:    "manual-commit",
		Transcript:  redact.AlreadyRedacted([]byte(transcript)),
		Prompts:     []string{prompt},
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	}))
}

type committedReaderStub struct {
	summary *CheckpointSummary
	readErr error
}

func (s *committedReaderStub) ListCheckpoints(context.Context) ([]CommittedInfo, error) {
	return nil, nil
}

func (s *committedReaderStub) ReadCheckpoint(context.Context, id.CheckpointID) (*CheckpointSummary, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.summary, nil
}

func (s *committedReaderStub) ReadSession(context.Context, SessionRef, ...ReadOption) (*SessionContent, error) {
	return nil, ErrCheckpointNotFound
}
