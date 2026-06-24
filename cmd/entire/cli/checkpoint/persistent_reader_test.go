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

func TestReadCheckpointNormalizesNilSummary(t *testing.T) {
	t.Parallel()

	reader := &committedReaderStub{}
	summary, err := ReadCheckpoint(context.Background(), reader, id.MustCheckpointID("111111111111"))
	require.Nil(t, summary)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

func TestReadCheckpointWrapsReaderError(t *testing.T) {
	t.Parallel()

	readerErr := errors.New("boom")
	reader := &committedReaderStub{readErr: readerErr}
	summary, err := ReadCheckpoint(context.Background(), reader, id.MustCheckpointID("111111111111"))
	require.Nil(t, summary)
	require.ErrorIs(t, err, readerErr)
	require.ErrorContains(t, err, "read persistent checkpoint")
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

	require.NoError(t, store.Write(ctx, Session{
		CheckpointID: cpID,
		SessionID:    "session-a",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("first transcript\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	require.NoError(t, store.Write(ctx, Session{
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

type committedReaderStub struct {
	summary *CheckpointSummary
	readErr error
}

func (s *committedReaderStub) Read(context.Context, id.CheckpointID) (*CheckpointSummary, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.summary, nil
}

func (s *committedReaderStub) ReadSessionContent(context.Context, id.CheckpointID, int) (*SessionContent, error) {
	return nil, ErrCheckpointNotFound
}

func (s *committedReaderStub) List(context.Context) ([]CheckpointInfo, error) {
	return nil, nil
}

func (s *committedReaderStub) ReadSessionMetadata(context.Context, id.CheckpointID, int) (*Metadata, error) {
	return nil, ErrCheckpointNotFound
}

func (s *committedReaderStub) ReadSessionPrompts(context.Context, id.CheckpointID, int) (string, error) {
	return "", ErrCheckpointNotFound
}

func (s *committedReaderStub) ReadSessionMetadataAndPrompts(context.Context, id.CheckpointID, int) (*Metadata, string, error) {
	return nil, "", ErrCheckpointNotFound
}
