package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupNoFileChangesSession records the session a snapshot is for: one that
// changed no files, so turn end never ran SaveStep — no steps, no
// FilesTouched, only a live transcript (researchTranscript).
func setupNoFileChangesSession(t *testing.T, sessionID string) *git.Repository {
	t.Helper()
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })
	head, err := repo.Head()
	require.NoError(t, err)

	transcriptPath := filepath.Join(t.TempDir(), "live.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(researchTranscript), 0o644))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, SaveSessionState(context.Background(), &SessionState{
		SessionID:           sessionID,
		BaseCommit:          head.Hash().String(),
		WorktreePath:        dir,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcriptPath,
		Phase:               session.PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
	}))
	return repo
}

const researchTranscript = `{"type":"human","message":{"content":"research the retry design"}}
{"type":"assistant","message":{"content":"Reading the code."}}
{"type":"human","message":{"content":"use key ` + taskTranscriptSecret + `"}}
`

// A session that changed no files gets a checkpoint of its transcript —
// redacted, including the turn still in progress — while its state is left
// exactly as it was.
func TestCreateSnapshotCheckpoint_WritesRedactedCheckpointWithoutTouchingState(t *testing.T) {
	sessionID := "2026-10-03-snapshot-research"
	repo := setupNoFileChangesSession(t, sessionID)

	before, err := LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)

	checkpointID, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.NoError(t, err)
	require.False(t, checkpointID.IsEmpty())

	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(context.Background(), checkpointID, 0)
	require.NoError(t, err)
	transcript := string(content.Transcript)
	assert.Contains(t, transcript, "use key", "snapshot must include the live transcript's newest turn")
	assert.NotContains(t, transcript, taskTranscriptSecret, "snapshot transcript must be redacted")
	assert.Contains(t, transcript, "REDACTED")

	after, err := LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a snapshot must not save anything back to session state")
}

// Pending file changes belong to the next commit's checkpoint, which carries
// their attribution; a snapshot would only duplicate it.
func TestCreateSnapshotCheckpoint_RefusesPendingFileChanges(t *testing.T) {
	sessionID := "2026-10-03-snapshot-pending-files"
	// The fixture's SaveStep records test.txt in FilesTouched.
	repo, _ := setupCondensableSessionWithTranscript(t, sessionID)

	_, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.ErrorIs(t, err, ErrPendingFileChanges)

	checkpoints, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, checkpoints)
}

// Most agents report no per-tool file events, so an edit made earlier in the
// current turn is in the live transcript but not yet in FilesTouched, which
// SaveStep fills at turn end. The guard must still see it. A file written
// outside the worktree (an agent's plan file) is not a pending change.
func TestCreateSnapshotCheckpoint_SeesEditsFromTheCurrentTurn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writtenPath func(worktree string) string
		wantRefused bool
	}{
		{"edit inside the worktree", func(wt string) string { return filepath.Join(wt, "notes.md") }, true},
		{"plan file outside the worktree", func(string) string { return filepath.Join(t.TempDir(), "plan.md") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel: setupNoFileChangesSession uses t.Chdir.
			sessionID := "2026-10-03-snapshot-current-turn"
			setupNoFileChangesSession(t, sessionID)
			state, err := LoadSessionState(context.Background(), sessionID)
			require.NoError(t, err)
			require.Empty(t, state.FilesTouched, "premise: the edit has not reached FilesTouched")

			writeLine := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` +
				tc.writtenPath(state.WorktreePath) + `","content":"x"}}]}}` + "\n"
			require.NoError(t, os.WriteFile(state.TranscriptPath, []byte(researchTranscript+writeLine), 0o644))

			_, err = (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), sessionID)
			if tc.wantRefused {
				require.ErrorIs(t, err, ErrPendingFileChanges)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCreateSnapshotCheckpoint_UnknownSession(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	_, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), "no-such-session")
	require.ErrorContains(t, err, "session not found")
}

// A session with a turn-end step but no transcript and no files is the
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

// Hook-path condensation drops the transcript when runtime redaction fails, so
// a commit is never blocked on it. A snapshot exists for its transcript, so the
// same failure must be an error: not an ID for a transcript-less checkpoint,
// and not a misleading "nothing to checkpoint".
func TestCreateSnapshotCheckpoint_RedactionFailureIsAnError(t *testing.T) {
	// No t.Parallel: swaps the package-level redaction seam and uses t.Chdir.
	originalRedact := redactSessionJSONLBytes
	redactSessionJSONLBytes = func(context.Context, []byte) (redact.RedactedBytes, error) {
		return redact.RedactedBytes{}, errors.New("forced redaction failure")
	}
	t.Cleanup(func() { redactSessionJSONLBytes = originalRedact })

	sessionID := "2026-09-30-snapshot-redaction-failure"
	repo := setupNoFileChangesSession(t, sessionID)

	_, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.ErrorContains(t, err, "forced redaction failure")
	require.NotErrorIs(t, err, ErrNothingToCheckpoint)

	checkpoints, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, checkpoints, "no checkpoint may be written when the transcript could not be redacted")
}

// The per-session redaction prefix cache is written in two steps, so a snapshot
// must condense under the session's state lock, like a commit's condensation or
// a turn-end finalize. Probed from inside redaction: another writer trying to
// take the lock then must time out. (Racing two writers instead would pass by
// luck whenever the snapshot happened to be slow.)
func TestCreateSnapshotCheckpoint_RedactsUnderTheSessionLock(t *testing.T) {
	// No t.Parallel: swaps the package-level redaction seam and uses t.Chdir.
	sessionID := "2026-10-03-snapshot-lock"
	setupNoFileChangesSession(t, sessionID)

	originalRedact := redactSessionJSONLBytes
	t.Cleanup(func() { redactSessionJSONLBytes = originalRedact })
	var probed, otherWriterGotLock bool
	var probeErr error
	redactSessionJSONLBytes = func(ctx context.Context, b []byte) (redact.RedactedBytes, error) {
		if !probed {
			probed = true
			// A separate goroutine: the gate is reentrant per goroutine.
			done := make(chan error, 1)
			go func() {
				probeCtx := WithSessionLockWait(context.Background(), 200*time.Millisecond)
				done <- MutateSessionState(probeCtx, sessionID, func(*SessionState) error {
					otherWriterGotLock = true
					return ErrMutationSkip
				})
			}()
			probeErr = <-done
		}
		return originalRedact(ctx, b)
	}

	_, err := (&ManualCommitStrategy{}).CreateSnapshotCheckpoint(context.Background(), sessionID)
	require.NoError(t, err)
	require.True(t, probed, "redaction never ran, so the probe proves nothing")
	assert.False(t, otherWriterGotLock, "another writer took the session lock while the snapshot was redacting")
	assert.ErrorContains(t, probeErr, "acquire state lock")
}
