package strategy

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

// reservedSession is the session the tests here reserve a checkpoint ID for.
const reservedSession = "sess-reserved"

// saveReservedSession saves reservedSession, stamped on its shape alone (a
// running subagent), holding a reserved checkpoint ID.
func saveReservedSession(t *testing.T, s *ManualCommitStrategy, dir string, reserved id.CheckpointID) {
	t.Helper()
	saveReadOnlySession(t, s, dir, reservedSession, func(st *SessionState) {
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
	saveReservedSession(t, s, dir, reserved)

	stageFile(t, dir, "first.txt")
	first := prepareMessage(t, "first\n")
	require.Equal(t, []string{reserved.String()}, checkpointIDs(first), "the first commit takes the reservation: %q", first)
	testutil.GitCommit(t, dir, first)

	stageFile(t, dir, "second.txt")
	second := prepareMessage(t, "second\n")
	require.Len(t, checkpointIDs(second), 1, "%q", second)
	assert.NotEqual(t, reserved.String(), checkpointIDs(second)[0], "the next commit must not carry the reservation the first one does")

	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx), "the first commit's post-commit")
	state, err := s.loadSessionState(ctx, reservedSession)
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
	state, err = s.loadSessionState(ctx, reservedSession)
	require.NoError(t, err)
	assert.Equal(t, checkpointIDs(second)[0], state.LastCheckpointID.String(), "the second commit's checkpoint is written")
	assert.Equal(t, 0, transcriptStartOf(t, s, dir, reserved), "the first commit's checkpoint still holds its own work")
	assert.Equal(t, 2, transcriptStartOf(t, s, dir, id.MustCheckpointID(checkpointIDs(second)[0])), "the second commit's holds the work since")
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)), "each post-commit releases its entries")
}

// A message saved from a commit already made (`commit -F`, or `-C` of it)
// names that commit's stamp. Before that commit's post-commit takes the stamp,
// prepare took it for its own and added no trailer, so the two commits named
// one checkpoint. The next commit now gets a trailer of its own.
func TestPrepareCommitMsg_SavedMessageNamingAnInFlightStampGetsItsOwn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := NewManualCommitStrategy()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")
	saveReservedSession(t, s, dir, reserved)

	stageFile(t, dir, "first.txt")
	first := prepareMessage(t, "first\n")
	require.Equal(t, []string{reserved.String()}, checkpointIDs(first))
	testutil.GitCommit(t, dir, first)

	stageFile(t, dir, "second.txt")
	second := prepareMessage(t, first)
	ids := checkpointIDs(second)
	require.Len(t, ids, 2, "%q", second)
	assert.Equal(t, reserved.String(), ids[0], "the saved message keeps its trailer")
	assert.NotEqual(t, reserved.String(), ids[1], "and the commit gets a trailer of its own: %q", second)
}

// A commit tried again after its first attempt was aborted (the editor quit,
// a commit-msg hook refused) is prepared on the same HEAD. The stamp left by
// the first attempt is its own, so it keeps the reservation.
func TestPrepareCommitMsg_RetryAfterAbortedAttemptKeepsTheReservation(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := NewManualCommitStrategy()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")
	saveReservedSession(t, s, dir, reserved)

	stageFile(t, dir, "work.txt")
	require.Equal(t, []string{reserved.String()}, checkpointIDs(prepareMessage(t, "attempt\n")))
	require.Equal(t, []string{reserved.String()}, checkpointIDs(prepareMessage(t, "retry\n")), "the retry keeps the reservation")
}

// The reservation is another commit's only while that commit's post-commit
// may still condense into it: from the moment the commit carrying it is made
// until its post-commit is done, through the take that removes its stamp.
func TestStampCarriedInFlight_UntilItsPostCommitIsDone(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, other := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved))
	assert.False(t, stampCarriedInFlight(ctx, reserved), "stamped on HEAD: the commit is not made yet")
	commit := commitWith(t, dir, "first", reserved)
	assert.True(t, stampCarriedInFlight(ctx, reserved), "made, its post-commit not run")
	assert.False(t, stampCarriedInFlight(ctx, other), "never stamped")

	records := takeStampRecords(ctx, commit)
	require.True(t, records.stamped[reserved])
	assert.True(t, stampCarriedInFlight(ctx, reserved), "taken, its post-commit not done")
	commitWith(t, dir, "second")
	assert.True(t, stampCarriedInFlight(ctx, reserved), "with a later commit made on top of it")

	releaseStampedSessions(ctx, records.sessions)
	assert.False(t, stampCarriedInFlight(ctx, reserved), "its post-commit is done")
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)))
}

// A reserved ID stamped on a commit that never happened, with other commits
// made since, is no commit's: the next commit takes it, as before.
func TestStampCarriedInFlight_NotForAnAbandonedStamp(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved), "an attempt that is abandoned")
	commitWith(t, dir, "unrelated")
	assert.False(t, stampCarriedInFlight(ctx, reserved))
}

// The entry an earlier post-commit's take left stays while that post-commit
// runs, though a later post-commit's take comes first. A third commit
// prepared meanwhile then still finds the reservation carried.
func TestTakeStampRecords_KeepsAnEntryWhileItsTakerRuns(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, next := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved))
	first := takeStampRecords(ctx, commitWith(t, dir, "first", reserved))
	require.True(t, recordStampedTrailer(ctx, next))
	second := takeStampRecords(ctx, commitWith(t, dir, "second", next))
	assert.NotContains(t, second.sessions, reserved)
	assert.True(t, stampCarriedInFlight(ctx, reserved), "the first commit's post-commit still runs")

	releaseStampedSessions(ctx, second.sessions)
	assert.True(t, stampCarriedInFlight(ctx, reserved))
	releaseStampedSessions(ctx, first.sessions)
	assert.False(t, stampCarriedInFlight(ctx, reserved))
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)))
}

// A post-commit that stopped before releasing its entries leaves them only
// until the next post-commit's take, and the reservation counts as carried
// no longer: the next commit takes it, as when no commit carried it.
func TestTakeStampRecords_DropsAnEntryWhoseTakerStopped(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, next := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")

	require.True(t, recordStampedTrailer(ctx, reserved))
	takeStampRecords(ctx, commitWith(t, dir, "first", reserved))
	require.True(t, stampCarriedInFlight(ctx, reserved))
	stopTaker(t, reserved)
	assert.False(t, stampCarriedInFlight(ctx, reserved), "its post-commit stopped")

	require.True(t, recordStampedTrailer(ctx, next))
	records := takeStampRecords(ctx, commitWith(t, dir, "second", next))
	releaseStampedSessions(ctx, records.sessions)
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)), "the next take drops it")
}

// stopTaker marks cpID's sessions entry as taken by a post-commit that has
// stopped: the process start is not this process's, and it is old enough
// that the fallback for an unknown process does not count it as running
// either.
func stopTaker(t *testing.T, cpID id.CheckpointID) {
	t.Helper()
	root := mustPerWorktreeGitRoot(t)
	entries := readStampedSessionsIn(root)
	gone := proclive.Identity{PID: os.Getpid(), Start: "not-this-process"}
	for i := range entries {
		if entries[i].CheckpointID == cpID {
			entries[i].Taken = &stampTaker{Process: &gone, At: time.Now().Add(-2 * takenEntryLife)}
		}
	}
	require.NoError(t, writeStampedSessionsIn(root, entries))
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

// Prepare reads the reservation again after finding no commit carries it.
// The earlier commit's post-commit may have condensed the session and
// released its entry between prepare loading the session and that check;
// the reservation is then the earlier commit's checkpoint, not this one's.
func TestCheckpointIDForSessions_ReadsTheReservationAgain(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	s := NewManualCommitStrategy()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")
	saveReservedSession(t, s, dir, reserved)
	snapshot, err := s.loadSessionState(ctx, reservedSession)
	require.NoError(t, err)

	require.NoError(t, settleReservation())
	got, err := checkpointIDForSessions(ctx, []*SessionState{snapshot})
	require.NoError(t, err)
	assert.NotEqual(t, reserved, got)
	assert.False(t, got.IsEmpty())
}

// settleReservation clears reservedSession's saved reservation, as its
// condensation does.
func settleReservation() error {
	state, err := LoadSessionState(context.Background(), reservedSession)
	if err != nil {
		return err
	}
	if state == nil {
		return errors.New("no saved state for " + reservedSession)
	}
	state.ClearCondensationAttempt()
	return SaveSessionState(context.Background(), state)
}

// heldReservation saves a session holding reserved, carried by a commit whose
// post-commit, this process, has taken its stamp, and returns the session as
// a post-commit's snapshot holds it.
func heldReservation(t *testing.T, dir string, reserved id.CheckpointID) *SessionState {
	t.Helper()
	ctx := context.Background()
	s := NewManualCommitStrategy()
	saveReservedSession(t, s, dir, reserved)
	require.True(t, recordStampedTrailer(ctx, reserved))
	takeStampRecords(ctx, commitWith(t, dir, "first", reserved))
	snapshot, err := s.loadSessionState(ctx, reservedSession)
	require.NoError(t, err)
	return snapshot
}

// A post-commit whose session still reserves the checkpoint ID an earlier
// commit carries waits while that commit's post-commit runs, until it has
// condensed the session.
func TestAwaitEarlierCondensations_UntilTheSessionIsCondensed(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, next := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")
	snapshot := heldReservation(t, dir, reserved)

	const condenseAfter = 200 * time.Millisecond
	condensed := make(chan error, 1)
	go func() {
		time.Sleep(condenseAfter)
		condensed <- settleReservation()
	}()
	start := time.Now()
	assert.True(t, awaitEarlierCondensations(ctx, []*SessionState{snapshot}, next, nil))
	assert.GreaterOrEqual(t, time.Since(start), condenseAfter)
	require.NoError(t, <-condensed)
	now, read := savedReservation(ctx, reservedSession)
	assert.True(t, read)
	assert.True(t, now.IsEmpty(), "it returned once the session was condensed")
}

// It never waits for its own stamp, for its own checkpoint, or for a
// post-commit that has stopped.
func TestAwaitEarlierCondensations_NotWhenNothingIsComing(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, next := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")
	snapshot := heldReservation(t, dir, reserved)
	sessions := []*SessionState{snapshot}

	assert.False(t, awaitEarlierCondensations(ctx, sessions, reserved, nil), "its own checkpoint")
	assert.False(t, awaitEarlierCondensations(ctx, sessions, next, map[id.CheckpointID]stampedSessions{reserved: {}}), "its own stamp")
	stopTaker(t, reserved)
	assert.False(t, awaitEarlierCondensations(ctx, sessions, next, nil), "a stopped post-commit")
}

// The wait is bounded: a post-commit that takes too long to condense the
// session is not waited for any longer.
func TestAwaitEarlierCondensations_GivesUpAtTheBound(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	reserved, next := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")
	snapshot := heldReservation(t, dir, reserved)
	bound := earlierCondensationWait
	earlierCondensationWait = 200 * time.Millisecond
	t.Cleanup(func() { earlierCondensationWait = bound })

	start := time.Now()
	assert.True(t, awaitEarlierCondensations(ctx, []*SessionState{snapshot}, next, nil))
	assert.GreaterOrEqual(t, time.Since(start), earlierCondensationWait)
	assert.Less(t, time.Since(start), bound)
}

// The later commit's post-commit can reach the session before the earlier
// commit's post-commit has condensed it. It waits for that condensation
// instead of skipping the session, which left the later commit naming a
// checkpoint that is never written.
func TestCommitHooks_LaterPostCommitWaitsForTheEarlierCondensation(t *testing.T) {
	ctx := context.Background()
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := NewManualCommitStrategy()
	reserved := id.MustCheckpointID("a1b2c3d4e5f6")
	saveReservedSession(t, s, dir, reserved)

	stageFile(t, dir, "first.txt")
	testutil.GitCommit(t, dir, prepareMessage(t, "first\n"))
	stageFile(t, dir, "second.txt")
	second := prepareMessage(t, "second\n")
	require.Len(t, checkpointIDs(second), 1, "%q", second)
	next := id.MustCheckpointID(checkpointIDs(second)[0])
	require.NotEqual(t, reserved, next)

	// The first commit's post-commit has taken its records and not condensed
	// the session yet when the second commit lands and its post-commit runs.
	var landed atomic.Bool
	later := make(chan error, 1)
	SetBeforeCondense(func(context.Context, []*SessionState) bool {
		if landed.CompareAndSwap(false, true) {
			testutil.GitCommit(t, dir, second)
			go func() { later <- NewManualCommitStrategy().PostCommit(ctx) }()
			time.Sleep(300 * time.Millisecond)
		}
		return false
	})
	t.Cleanup(func() { SetBeforeCondense(nil) })

	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx), "the first commit's post-commit")
	require.NoError(t, <-later, "the second commit's post-commit")
	state, err := s.loadSessionState(ctx, reservedSession)
	require.NoError(t, err)
	assert.Equal(t, next, state.LastCheckpointID, "the second commit's checkpoint is written")
	assert.Equal(t, 0, transcriptStartOf(t, s, dir, reserved), "the first commit's checkpoint holds its own work")
	assert.Equal(t, 2, transcriptStartOf(t, s, dir, next))
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)), "each post-commit releases its entries")
}
