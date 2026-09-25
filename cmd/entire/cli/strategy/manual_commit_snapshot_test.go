package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A snapshot checkpoint is written — redacted, with the live transcript's
// latest turn — while the session's own checkpoint window is left exactly as
// it was, so the next commit condenses the same range.
func TestCreateSnapshotCheckpoint_WritesRedactedCheckpointWithoutTouchingState(t *testing.T) {
	sessionID := "2026-09-25-snapshot-checkpoint"
	repo, state := setupCondensableSessionWithTranscript(t, sessionID)

	// Mid-turn: the agent's live transcript has moved past the last SaveStep,
	// and the new turn carries a secret.
	liveTranscript := filepath.Join(t.TempDir(), "live.jsonl")
	require.NoError(t, os.WriteFile(liveTranscript, []byte(`{"type":"human","message":{"content":"dispatch a subagent"}}
{"type":"assistant","message":{"content":"On it."}}
{"type":"human","message":{"content":"use key `+taskTranscriptSecret+`"}}
`), 0o644))
	state.TranscriptPath = liveTranscript
	state.Phase = session.PhaseActive
	require.NoError(t, SaveSessionState(context.Background(), state))

	before, err := LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	checkpointID, err := s.CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.NoError(t, err)
	require.False(t, checkpointID.IsEmpty())

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	require.NoError(t, err)
	transcript := string(content.Transcript)
	assert.Contains(t, transcript, "use key", "snapshot must include the live transcript's newest turn")
	assert.NotContains(t, transcript, taskTranscriptSecret, "snapshot transcript must be redacted")
	assert.Contains(t, transcript, "REDACTED")

	after, err := LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a snapshot must not save anything back to session state")
}

func TestCreateSnapshotCheckpoint_UnknownSession(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	_, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), "no-such-session")
	require.ErrorContains(t, err, "session not found")
}

// A session with a shadow branch but no transcript and no files is the
// condensation skip gate; the snapshot reports it rather than returning an ID
// for a checkpoint that was never written.
func TestCreateSnapshotCheckpoint_NothingToCheckpoint(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-09-25-snapshot-empty"
	metadataDir := ".entire/metadata/" + sessionID
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dummy.txt"), []byte("x"), 0o644))
	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:     sessionID,
		NewFiles:      []string{"dummy.txt"},
		MetadataDir:   metadataDir,
		CommitMessage: "checkpoint without transcript",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.FilesTouched = nil
	require.NoError(t, s.saveSessionState(context.Background(), state))

	_, err = s.CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.ErrorIs(t, err, ErrNothingToCheckpoint)
}

// A snapshot has no commit, so HEAD still predates the agent's uncommitted
// work. Commit attribution compares the shadow tree with HEAD and would count
// that work as human removals; the snapshot must record no attribution at all.
func TestCreateSnapshotCheckpoint_RecordsNoCommitAttribution(t *testing.T) {
	sessionID := "2026-09-25-snapshot-attribution"
	// The fixture's shadow tree holds an uncommitted agent edit to test.txt.
	repo, _ := setupCondensableSessionWithTranscript(t, sessionID)

	checkpointID, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.NoError(t, err)

	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(context.Background(), checkpointID, 0)
	require.NoError(t, err)
	assert.Nil(t, content.Metadata.Attribution, "a commitless snapshot must not carry commit attribution")
}
