package strategy

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

// saveReservedSession saves a session, stamped on its shape alone (a running
// subagent), that holds a reserved checkpoint ID.
func saveReservedSession(t *testing.T, s *ManualCommitStrategy, dir, sessionID string, reserved id.CheckpointID) {
	t.Helper()
	saveReadOnlySession(t, s, dir, sessionID, func(st *SessionState) {
		startReadOnlySubagent(st)
		st.BeginCondensationAttempt(reserved)
	})
}

// stageFile writes name and stages it.
func stageFile(t *testing.T, dir, name string) {
	t.Helper()
	testutil.WriteFile(t, dir, name, name)
	testutil.GitAdd(t, dir, name)
}

// transcriptStartOf returns where cpID's first session's transcript range
// starts.
func transcriptStartOf(t *testing.T, s *ManualCommitStrategy, dir string, cpID id.CheckpointID) int {
	t.Helper()
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()
	store, err := s.getPersistentStore(context.Background(), repo)
	require.NoError(t, err)
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err, "checkpoint %s", cpID)
	return content.Metadata.CheckpointTranscriptStart
}

// A session's reserved checkpoint ID goes to one commit. The next commit,
// prepared before that commit's post-commit has condensed the session, used
// to get it too: it named the first commit's checkpoint, and its post-commit
// wrote the session's newer work over that checkpoint (#3069). It now gets a
// checkpoint of its own.
func TestCommitHooks_NextCommitPreparedBeforePostCommitDoesNotShareTheReservation(t *testing.T) {
	ctx := context.Background()
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := NewManualCommitStrategy()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")
	saveReservedSession(t, s, dir, "sess-reserved", reserved)

	stageFile(t, dir, "first.txt")
	first := prepareMessage(t, "first\n")
	require.Equal(t, []string{reserved.String()}, checkpointIDs(first), "the first commit takes the reservation: %q", first)
	testutil.GitCommit(t, dir, first)

	stageFile(t, dir, "second.txt")
	second := prepareMessage(t, "second\n")
	require.Len(t, checkpointIDs(second), 1, "%q", second)
	assert.NotEqual(t, reserved.String(), checkpointIDs(second)[0], "the next commit must not carry the reservation the first one does")

	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx), "the first commit's post-commit")
	state, err := s.loadSessionState(ctx, "sess-reserved")
	require.NoError(t, err)
	require.Equal(t, reserved, state.LastCheckpointID, "the first commit condenses into the reservation")
	require.True(t, state.PendingCondensationID().IsEmpty(), "and settles it")

	// The session works on before the second commit's post-commit.
	f, err := os.OpenFile(state.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"human","message":{"content":"next prompt"}}` + "\n")
	require.NoError(t, f.Close())
	require.NoError(t, err)

	testutil.GitCommit(t, dir, second)
	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx), "the second commit's post-commit")
	state, err = s.loadSessionState(ctx, "sess-reserved")
	require.NoError(t, err)
	assert.Equal(t, checkpointIDs(second)[0], state.LastCheckpointID.String(), "the second commit's checkpoint is written")
	assert.Equal(t, 0, transcriptStartOf(t, s, dir, reserved), "the first commit's checkpoint still holds its own work")
	assert.Equal(t, 2, transcriptStartOf(t, s, dir, id.MustCheckpointID(checkpointIDs(second)[0])), "the second commit's holds the work since")
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)), "each post-commit releases its entries")
}

// A commit tried again after its first attempt was aborted (the editor quit,
// a commit-msg hook refused) is prepared on the same HEAD. The stamp left by
// the first attempt is its own, so it keeps the reservation.
func TestPrepareCommitMsg_RetryAfterAbortedAttemptKeepsTheReservation(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := NewManualCommitStrategy()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")
	saveReservedSession(t, s, dir, "sess-reserved", reserved)

	stageFile(t, dir, "work.txt")
	require.Equal(t, []string{reserved.String()}, checkpointIDs(prepareMessage(t, "attempt\n")))
	require.Equal(t, []string{reserved.String()}, checkpointIDs(prepareMessage(t, "retry\n")), "the retry keeps the reservation")
}

// The reservation is another commit's only while that commit's post-commit
// may still condense into it: from the moment the commit carrying it is made
// until its post-commit is done, through the take that removes its stamp.
func TestReservationCarriedInFlight_UntilItsPostCommitIsDone(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, other := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved))
	assert.False(t, reservationCarriedInFlight(ctx, reserved), "stamped on HEAD: the commit is not made yet")
	commit := commitWith(t, dir, "first", reserved)
	assert.True(t, reservationCarriedInFlight(ctx, reserved), "made, its post-commit not run")
	assert.False(t, reservationCarriedInFlight(ctx, other), "never stamped")

	records := takeStampRecords(ctx, commit)
	require.True(t, records.stamped[reserved])
	assert.True(t, reservationCarriedInFlight(ctx, reserved), "taken, its post-commit not done")
	commitWith(t, dir, "second")
	assert.True(t, reservationCarriedInFlight(ctx, reserved), "with a later commit made on top of it")

	releaseStampedSessions(ctx, records.sessions)
	assert.False(t, reservationCarriedInFlight(ctx, reserved), "its post-commit is done")
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)))
}

// A reserved ID stamped on a commit that never happened, with other commits
// made since, is no commit's: the next commit takes it, as before.
func TestReservationCarriedInFlight_NotForAnAbandonedStamp(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved), "an attempt that is abandoned")
	commitWith(t, dir, "unrelated")
	assert.False(t, reservationCarriedInFlight(ctx, reserved))
}

// A post-commit that never gets to release its entries leaves them only
// until the next post-commit's take, which drops entries without a stamp.
func TestTakeStampRecords_DropsEntriesAnEarlierTakeLeft(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, next := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved))
	first := commitWith(t, dir, "first", reserved)
	takeStampRecords(ctx, first)
	require.True(t, reservationCarriedInFlight(ctx, reserved))

	require.True(t, recordStampedTrailer(ctx, next))
	second := commitWith(t, dir, "second", next)
	records := takeStampRecords(ctx, second)
	assert.NotContains(t, records.sessions, reserved)
	assert.False(t, reservationCarriedInFlight(ctx, reserved), "the next take drops it")
	releaseStampedSessions(ctx, records.sessions)
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)))
}

// Prepare passes over a reserved ID another commit carries and takes the next
// session's, or a fresh one.
func TestCheckpointIDForSessions_SkipsAReservationCarriedInFlight(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	carried, free := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")
	require.True(t, recordStampedTrailer(ctx, carried))
	commitWith(t, dir, "first", carried)

	holding := &SessionState{SessionID: "carried"}
	holding.BeginCondensationAttempt(carried)
	other := &SessionState{SessionID: "free"}
	other.BeginCondensationAttempt(free)

	got, err := checkpointIDForSessions(ctx, []*SessionState{holding, other})
	require.NoError(t, err)
	assert.Equal(t, free, got)

	got, err = checkpointIDForSessions(ctx, []*SessionState{holding})
	require.NoError(t, err)
	assert.NotEqual(t, carried, got)
	assert.False(t, got.IsEmpty())
}
