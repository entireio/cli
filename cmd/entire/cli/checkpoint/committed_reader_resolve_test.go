package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

func newCommittedReaderTestRepo(t *testing.T) *git.Repository {
	t.Helper()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return repo
}

func TestNewCommittedReaderUsesV1Store(t *testing.T) {
	t.Parallel()

	repo := newCommittedReaderTestRepo(t)
	reader, err := NewCommittedReader(context.Background(), repo, CommittedReaderOptions{})
	require.NoError(t, err)
	require.IsType(t, &GitStore{}, reader)
}

func TestReadRawSessionLogForCheckpointUsesV1LatestSession(t *testing.T) {
	t.Parallel()

	repo := newCommittedReaderTestRepo(t)
	store := NewGitStore(repo)
	ctx := context.Background()
	cpID := id.MustCheckpointID("555555555555")

	require.NoError(t, store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-a",
		CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-session-a"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	require.NoError(t, store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-b",
		CreatedAt:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"text":"from-session-b"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	logContent, sessionID, err := ReadRawSessionLogForCheckpoint(ctx, store, cpID)
	require.NoError(t, err)
	require.Equal(t, "session-b", sessionID)
	require.Contains(t, string(logContent), "from-session-b")
	require.NotContains(t, string(logContent), "from-session-a")
}
