package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// A salvage interrupted after reserving its checkpoint ID leaves the
// reservation pending; prepare-commit-msg then reuses it for a commit whose
// post-commit condensation also fails to write. When the next salvage finds no
// transcript, it must still write that checkpoint: a commit names it, so
// skipping (as for an ID the salvage minted itself) would leave the trailer
// dangling with recovery cleared.
func TestCondenseSessionByID_WritesPreexistingReservationWithoutTranscript(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	dir := setupGitRepo(t)
	t.Chdir(dir)
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	s := &ManualCommitStrategy{}
	sid := "reserved-then-committed"
	setupSessionWithCheckpoint(t, s, repo, dir, sid)
	state, err := s.loadSessionState(t.Context(), sid)
	require.NoError(t, err)
	ended := time.Now().Add(-48 * time.Hour)
	state.Phase, state.EndedAt = session.PhaseEnded, &ended
	state.AgentType = agent.AgentTypeClaudeCode
	state.TranscriptPath = filepath.Join(dir, ".entire", "metadata", sid, paths.TranscriptFileName)
	reserved, created, err := reserveDoctorCondensationAttempt(t.Context(), state)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, s.saveSessionState(t.Context(), state))

	testutil.GitAdd(t, dir, "test.txt")
	msgPath := filepath.Join(dir, "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgPath, []byte("commit pending work\n"), 0o600))
	require.NoError(t, s.PrepareCommitMsg(t.Context(), msgPath, "message"))
	msg, err := os.ReadFile(msgPath)
	require.NoError(t, err)
	stamped, found := trailers.ParseCheckpoint(string(msg))
	require.True(t, found, "prepare-commit-msg must stamp a trailer")
	require.Equal(t, reserved, stamped, "prepare-commit-msg reuses the pending reservation")
	// git commits; post-commit never persists the checkpoint.
	commitFilesWithTrailer(t, repo, dir, stamped.String(), "test.txt")

	// The only transcript is gone by the next salvage.
	require.NoError(t, os.Remove(state.TranscriptPath))
	require.NoError(t, s.CondenseSessionByID(t.Context(), sid))

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	summary, err := store.Read(t.Context(), reserved)
	require.NoError(t, err)
	require.NotNil(t, summary, "a commit names reservation %s; the salvage must write it even without a transcript", reserved)
}

// An ID the salvage minted itself is named by no commit, so with no transcript
// nothing is written and the session is settled.
func TestCondenseSessionByID_SkipsMintedIDWithoutTranscript(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	dir := setupGitRepo(t)
	t.Chdir(dir)
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	s := &ManualCommitStrategy{}
	sid := "minted-no-transcript"
	setupSessionWithCheckpoint(t, s, repo, dir, sid)
	state, err := s.loadSessionState(t.Context(), sid)
	require.NoError(t, err)
	ended := time.Now().Add(-48 * time.Hour)
	state.Phase, state.EndedAt = session.PhaseEnded, &ended
	state.AgentType = agent.AgentTypeClaudeCode
	state.TranscriptPath = filepath.Join(dir, ".entire", "metadata", sid, paths.TranscriptFileName)
	require.NoError(t, s.saveSessionState(t.Context(), state))
	require.NoError(t, os.Remove(state.TranscriptPath))

	require.NoError(t, s.CondenseSessionByID(t.Context(), sid))

	after, err := s.loadSessionState(t.Context(), sid)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.True(t, after.FullyCondensed)
	require.True(t, after.LastCheckpointID.IsEmpty(), "no transcript-less checkpoint may be written for a minted ID")
	require.False(t, IsCondensableEndedSession(after), "the session must not be nominated again")
}
