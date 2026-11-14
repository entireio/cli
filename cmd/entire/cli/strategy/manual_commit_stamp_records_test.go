package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// Uses t.Chdir — do NOT add t.Parallel().

// takeInheritedTrailers takes the stamp records as post-commit does for a
// commit on parent, and returns the trailers recorded as inherited.
func takeInheritedTrailers(ctx context.Context, parent string) map[id.CheckpointID]bool {
	return takeStampRecords(ctx, commitOn(parent, "")).inherited
}

// commitOn is a commit on parent with message, as post-commit reads it, that
// no prepare can have run on.
func commitOn(parent, message string) *object.Commit {
	c := &object.Commit{Hash: plumbing.NewHash(strings.Repeat("c", 40)), Message: message}
	if parent != "" {
		c.ParentHashes = []plumbing.Hash{plumbing.NewHash(parent)}
	}
	return c
}

// commitWith commits the staged changes with message carrying cpIDs and
// returns the commit, as post-commit reads it. It commits through git, which
// writes HEAD's reflog as a real commit does.
func commitWith(t *testing.T, dir, subject string, cpIDs ...id.CheckpointID) *object.Commit {
	t.Helper()
	msg := subject + "\n"
	for _, cpID := range cpIDs {
		msg = addCheckpointTrailer(msg, cpID)
	}
	testutil.WriteFile(t, dir, subject+".txt", subject)
	testutil.GitAdd(t, dir, subject+".txt")
	testutil.RunGit(t, dir, "commit", "-q", "-m", msg)
	return headCommit(t, dir)
}

func mustPerWorktreeGitRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := perWorktreeGitRoot(context.Background())
	require.NoError(t, err)
	return root
}

// waitBriefly gives a hook blocked on the lock time to proceed if it wrongly
// does not wait.
func waitBriefly() <-chan time.Time {
	return time.After(200 * time.Millisecond)
}

// git runs post-commit after the commit is made, so the next commit in the
// worktree can start, and its prepare stamp, before this post-commit takes
// the records. Post-commit used to take every stamp, the next commit's too,
// which then kept a trailer naming a checkpoint that is never written. It now
// takes only the stamps its commit carries, keeps the ones stamped on top of
// its commit, and drops the rest: stamps of commits that never happened.
func TestTakeStampRecords_KeepsTheNextCommitsStamp(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	first, second, abandoned := id.MustCheckpointID("a1a1a1a1a1a1"), id.MustCheckpointID("b2b2b2b2b2b2"), id.MustCheckpointID("c3c3c3c3c3c3")

	require.True(t, recordStampedTrailer(withLinkedSessions(ctx, []string{"sess-first"}), first))
	require.True(t, recordStampedTrailer(ctx, abandoned), "a commit on the same parent that never happens")
	firstCommit := commitWith(t, dir, "first", first)
	require.True(t, recordStampedTrailer(withLinkedSessions(ctx, []string{"sess-second"}), second), "the next commit's prepare, before the first post-commit")

	records := takeStampRecords(ctx, firstCommit)
	assert.Equal(t, map[id.CheckpointID]bool{first: true}, records.stamped, "only the stamps the commit carries are its")
	assert.Equal(t, []string{"sess-first"}, records.sessions[first].SessionIDs)
	assert.NotContains(t, records.sessions, second)
	assert.Equal(t, map[id.CheckpointID]bool{second: true}, readStampedTrailers(ctx), "the next commit's stamp is kept, the abandoned one dropped")

	secondCommit := commitWith(t, dir, "second", second)
	records = takeStampRecords(ctx, secondCommit)
	assert.Equal(t, map[id.CheckpointID]bool{second: true}, records.stamped, "the next commit's post-commit finds its stamp")
	assert.Equal(t, []string{"sess-second"}, records.sessions[second].SessionIDs, "and the sessions its prepare linked")
	assert.Empty(t, readStampedTrailers(ctx))
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)))
}

// A post-commit can fall more than one commit behind: it reads its commit,
// then later commits land and the one after them prepares before it takes the
// records. It keeps every stamp prepared on its commit or on one landed on top
// of it since, so each of those commits' post-commits still finds its own.
func TestTakeStampRecords_KeepsStampsOfCommitsLandedSince(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	abandoned := id.MustCheckpointID("a0a0a0a0a0a0")
	first, second, third := id.MustCheckpointID("a1a1a1a1a1a1"), id.MustCheckpointID("b2b2b2b2b2b2"), id.MustCheckpointID("d4d4d4d4d4d4")

	require.True(t, recordStampedTrailer(ctx, abandoned), "a commit on the same parent that never happens")
	require.True(t, recordStampedTrailer(ctx, first))
	firstCommit := commitWith(t, dir, "first", first)
	require.True(t, recordStampedTrailer(ctx, second))
	secondCommit := commitWith(t, dir, "second", second)
	require.True(t, recordStampedTrailer(ctx, third), "the third commit's prepare, on the second")

	assert.Equal(t, map[id.CheckpointID]bool{first: true}, takeStampRecords(ctx, firstCommit).stamped)
	assert.Equal(t, map[id.CheckpointID]bool{second: true, third: true}, readStampedTrailers(ctx), "the later commits' stamps are kept, the abandoned one dropped")

	assert.Equal(t, map[id.CheckpointID]bool{second: true}, takeStampRecords(ctx, secondCommit).stamped)
	thirdCommit := commitWith(t, dir, "third", third)
	assert.Equal(t, map[id.CheckpointID]bool{third: true}, takeStampRecords(ctx, thirdCommit).stamped, "the third commit's post-commit finds its stamp")
	assert.Empty(t, readStampedTrailers(ctx))
	assert.Empty(t, readStampedSessionsIn(mustPerWorktreeGitRoot(t)))
}

// When more commits land than HEAD's walk reaches, post-commit cannot tell a
// later commit's stamp or marker from an abandoned one, so it keeps them all
// and takes only its own.
func TestTakeStampRecords_KeepsAllWhenLaterCommitsAreOutOfReach(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	first, later, nextSquashed := id.MustCheckpointID("a1a1a1a1a1a1"), id.MustCheckpointID("e9e9e9e9e9e9"), id.MustCheckpointID("a7a7a7a7a7a7")

	require.True(t, recordStampedTrailer(ctx, first))
	firstCommit := commitWith(t, dir, "first", first)
	for i := range maxCommitsInFlight {
		commitWith(t, dir, fmt.Sprintf("landed-%d", i))
	}
	require.True(t, recordStampedTrailer(ctx, later), "a prepare far above the first commit")
	recordInheritedTrailers(ctx, []id.CheckpointID{nextSquashed})

	records := takeStampRecords(ctx, firstCommit)
	assert.Equal(t, map[id.CheckpointID]bool{first: true}, records.stamped)
	assert.Empty(t, records.inherited)
	assert.Equal(t, map[id.CheckpointID]bool{later: true}, readStampedTrailers(ctx), "the later stamp is kept")
	_, ok := readInheritedMarkerIn(mustPerWorktreeGitRoot(t))
	assert.True(t, ok, "and so is the later marker")
}

// A commit the walk cannot read stops it short in the same way: post-commit
// cannot tell whether HEAD moved elsewhere or later commits landed, so it keeps
// the stamps it does not carry.
func TestTakeStampRecords_KeepsAllWhenTheWalkCannotReadACommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	first, later := id.MustCheckpointID("a1a1a1a1a1a1"), id.MustCheckpointID("e8e8e8e8e8e8")

	require.True(t, recordStampedTrailer(ctx, first))
	firstCommit := commitWith(t, dir, "first", first)
	unreadable := commitWith(t, dir, "unreadable")
	commitWith(t, dir, "top")
	require.True(t, recordStampedTrailer(ctx, later), "a prepare on the top commit")

	hash := unreadable.Hash.String()
	gitDir := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--absolute-git-dir"))
	require.NoError(t, os.Remove(filepath.Join(gitDir, "objects", hash[:2], hash[2:])))

	assert.Equal(t, map[id.CheckpointID]bool{first: true}, takeStampRecords(ctx, firstCommit).stamped)
	assert.Equal(t, map[id.CheckpointID]bool{later: true}, readStampedTrailers(ctx), "the later stamp is kept")
}

// A stamp left on top of a commit belongs to a commit that may never happen
// either: the post-commit after that keeps it no longer.
func TestTakeStampRecords_DropsAKeptStampAtTheNextPostCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	first, abandoned, third := id.MustCheckpointID("a1a1a1a1a1a1"), id.MustCheckpointID("c3c3c3c3c3c3"), id.MustCheckpointID("d4d4d4d4d4d4")

	require.True(t, recordStampedTrailer(ctx, first))
	firstCommit := commitWith(t, dir, "first", first)
	require.True(t, recordStampedTrailer(ctx, abandoned), "prepared on top of the first commit, then abandoned")
	takeStampRecords(ctx, firstCommit)
	require.True(t, readStampedTrailers(ctx)[abandoned], "precondition: kept for its own post-commit")

	require.True(t, recordStampedTrailer(ctx, third))
	thirdCommit := commitWith(t, dir, "third", third)
	assert.Equal(t, map[id.CheckpointID]bool{third: true}, takeStampRecords(ctx, thirdCommit).stamped)
	assert.Empty(t, readStampedTrailers(ctx), "the abandoned stamp goes with the next post-commit")
}

// A stamp recorded by a binary that did not record the HEAD it stamped on
// keeps the old lifetime: the next post-commit consumes it.
func TestTakeStampRecords_StampWithoutHeadLastsOnePostCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	old := id.MustCheckpointID("e5e5e5e5e5e5")
	root := mustPerWorktreeGitRoot(t)
	require.NoError(t, writeStampedTrailersIn(root, []id.CheckpointID{old}))

	takeStampRecords(ctx, headCommit(t, dir))
	assert.Empty(t, readStampedTrailers(ctx))
}

// The next commit's prepare can also run before this post-commit takes the
// inherited marker. With nothing inherited it used to clear the marker, and a
// squash commit then lost its inherited trailers and redone paths; it now
// leaves a marker HEAD's commit recorded. Post-commit in turn leaves a marker
// the next commit's prepare recorded on top of its commit.
func TestInheritedTrailersMarker_SurvivesTheNextCommitsPrepare(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	squashed, nextSquashed := id.MustCheckpointID("f6f6f6f6f6f6"), id.MustCheckpointID("a7a7a7a7a7a7")

	recordInheritedMarker(ctx, []id.CheckpointID{squashed}, map[string]bool{"redone.txt": true})
	squashCommit := commitWith(t, dir, "squash", squashed)
	recordInheritedTrailers(ctx, nil) // the next commit's prepare, which inherited nothing

	records := takeStampRecords(ctx, squashCommit)
	assert.Equal(t, map[id.CheckpointID]bool{squashed: true}, records.inherited, "the squash keeps its inherited trailers")
	assert.Equal(t, map[string]bool{"redone.txt": true}, records.redone)

	plainCommit := commitWith(t, dir, "plain")
	recordInheritedTrailers(ctx, []id.CheckpointID{nextSquashed}) // the next commit's prepare, before this post-commit
	assert.Empty(t, takeStampRecords(ctx, plainCommit).inherited, "a marker prepared on top of the commit is not its")
	nextCommit := commitWith(t, dir, "next", nextSquashed)
	assert.Equal(t, map[id.CheckpointID]bool{nextSquashed: true}, takeStampRecords(ctx, nextCommit).inherited, "it is the next commit's")
}

// The marker in the same ordering: a squash commit's post-commit falls behind
// while a later commit lands and the one after it prepares. Neither prepare
// clears the squash's marker, and the squash's post-commit leaves a marker a
// prepare on a later commit recorded.
func TestInheritedTrailersMarker_SurvivesCommitsLandedSince(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	squashed, nextSquashed := id.MustCheckpointID("f6f6f6f6f6f6"), id.MustCheckpointID("a7a7a7a7a7a7")

	recordInheritedMarker(ctx, []id.CheckpointID{squashed}, map[string]bool{"redone.txt": true})
	squashCommit := commitWith(t, dir, "squash", squashed)
	recordInheritedTrailers(ctx, nil) // the next commit's prepare
	plainCommit := commitWith(t, dir, "plain")
	recordInheritedTrailers(ctx, nil) // the one after it, on the plain commit

	records := takeStampRecords(ctx, squashCommit)
	assert.Equal(t, map[id.CheckpointID]bool{squashed: true}, records.inherited, "the squash keeps its inherited trailers")
	assert.Equal(t, map[string]bool{"redone.txt": true}, records.redone)

	recordInheritedTrailers(ctx, []id.CheckpointID{nextSquashed}) // a squash prepared on the plain commit
	nextCommit := commitWith(t, dir, "next", nextSquashed)
	assert.Empty(t, takeStampRecords(ctx, plainCommit).inherited, "a marker recorded on a later commit is not this one's")
	assert.Equal(t, map[id.CheckpointID]bool{nextSquashed: true}, takeStampRecords(ctx, nextCommit).inherited, "it is the next squash's")
}

// A redo prepared on a commit's parent and then cancelled leaves a marker
// recorded on that parent. Once HEAD is reset back onto the commit, the marker
// looks like one the commit's own post-commit has yet to take, so an amend's
// prepare kept it and the amend took it: its own checkpoint counted as
// inherited and the amended work never reached it. HEAD's reflog shows the
// reset, so the amend's prepare clears the marker, as it did before markers
// could wait.
func TestInheritedTrailersMarker_ClearedAfterACancelledRedo(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	checkpointed := id.MustCheckpointID("b8b8b8b8b8b8")

	parent := commitWith(t, dir, "parent")
	commit := commitWith(t, dir, "checkpointed", checkpointed)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", parent.Hash.String())
	recordInheritedTrailers(ctx, []id.CheckpointID{checkpointed}) // the redo's prepare, then cancelled
	testutil.RunGit(t, dir, "reset", "-q", "--soft", commit.Hash.String())

	recordInheritedTrailers(ctx, nil) // the amend's prepare, which inherited nothing
	_, ok := readInheritedMarkerIn(mustPerWorktreeGitRoot(t))
	assert.False(t, ok, "the cancelled redo's marker is cleared")

	testutil.WriteFile(t, dir, "amended.txt", "amended")
	testutil.GitAdd(t, dir, "amended.txt")
	testutil.RunGit(t, dir, "commit", "-q", "--amend", "--no-edit")
	assert.Empty(t, takeStampRecords(ctx, headCommit(t, dir)).inherited, "the amend inherits nothing")
}

// Prepare's read-modify-write of the records and post-commit's take hold the
// stamp-records lock, so neither can interleave with the other or with a
// second post-commit: a stamp appended between a take's read and its removal
// was lost, a stamp written back after a take resurrected the record it had
// consumed, and two post-commits could each take half the records.
func TestStampRecords_WaitForTheLock(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	cpID := id.MustCheckpointID("b8b8b8b8b8b8")
	root := mustPerWorktreeGitRoot(t)

	release := lockStampRecords(ctx, root)
	stamped := make(chan bool)
	go func() { stamped <- recordStampedTrailer(ctx, cpID) }()
	select {
	case <-stamped:
		t.Fatal("prepare recorded its stamp while another hook held the lock")
	case <-waitBriefly():
	}
	assert.Empty(t, readStampedTrailers(ctx))
	release()
	require.True(t, <-stamped)

	commit := commitWith(t, dir, "locked", cpID)
	release = lockStampRecords(ctx, root)
	taken := make(chan stampRecords)
	go func() { taken <- takeStampRecords(ctx, commit) }()
	select {
	case <-taken:
		t.Fatal("post-commit took the records while another hook held the lock")
	case <-waitBriefly():
	}
	release()
	assert.Equal(t, map[id.CheckpointID]bool{cpID: true}, (<-taken).stamped)
}

// The stamp's sessions entry carries the HEAD prepare stamped on, even when
// prepare linked no session, so post-commit can tell the next commit's stamp
// from an abandoned one.
func TestRecordStampedTrailer_RecordsTheHeadItStampedOn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	cpID := id.MustCheckpointID("c9c9c9c9c9c9")
	require.True(t, recordStampedTrailer(ctx, cpID))

	entries := readStampedSessionsIn(mustPerWorktreeGitRoot(t))
	require.Len(t, entries, 1)
	assert.Equal(t, testutil.GetHeadHash(t, dir), entries[0].Head)
	assert.Empty(t, entries[0].SessionIDs)
	assert.Contains(t, trailers.ParseAllCheckpoints(addCheckpointTrailer("x\n", cpID)), cpID, "sanity: the carried check parses the trailer prepare writes")
}

// A stamp whose sessions entry cannot be written is not recorded: without the
// HEAD in it, an earlier commit's post-commit would drop the stamp, and the
// commit would keep a trailer for a checkpoint that is never written. Prepare
// then leaves the commit unlinked, as for any stamp it cannot record.
func TestRecordStampedTrailer_FailsWithoutItsSessionsEntry(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	kept, cpID := id.MustCheckpointID("d1d1d1d1d1d1"), id.MustCheckpointID("d2d2d2d2d2d2")
	require.True(t, recordStampedTrailer(ctx, kept))

	gitDir := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--absolute-git-dir"))
	sessionsPath := filepath.Join(gitDir, stampedSessionsFile)
	require.NoError(t, os.Remove(sessionsPath))
	require.NoError(t, os.Mkdir(sessionsPath, 0o700), "an entry nothing can write over")

	assert.False(t, recordStampedTrailer(ctx, cpID))
	assert.Equal(t, map[id.CheckpointID]bool{kept: true}, readStampedTrailers(ctx), "the stamp is not recorded")
}

// The same race through the hooks: the next commit is prepared before the
// first commit's post-commit runs. Both commits keep their checkpoints.
func TestCommitHooks_NextCommitPreparedBeforePostCommitGetsItsCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := NewManualCommitStrategy()
	saveReadOnlySession(t, s, dir, "sess-busy", startReadOnlySubagent)

	testutil.WriteFile(t, dir, "first.txt", "first")
	testutil.GitAdd(t, dir, "first.txt")
	first := prepareMessage(t, "first\n")
	require.Len(t, checkpointIDs(first), 1, "prepare stamps the session by its shape: %q", first)
	testutil.GitCommit(t, dir, first)

	testutil.WriteFile(t, dir, "second.txt", "second")
	testutil.GitAdd(t, dir, "second.txt")
	second := prepareMessage(t, "second\n")
	require.Len(t, checkpointIDs(second), 1, "%q", second)
	require.NotEqual(t, checkpointIDs(first), checkpointIDs(second))

	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx), "the first commit's post-commit")
	state, err := s.loadSessionState(ctx, "sess-busy")
	require.NoError(t, err)
	require.Equal(t, checkpointIDs(first)[0], state.LastCheckpointID.String())

	testutil.GitCommit(t, dir, second)
	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx), "the second commit's post-commit")
	state, err = s.loadSessionState(ctx, "sess-busy")
	require.NoError(t, err)
	assert.Equal(t, checkpointIDs(second)[0], state.LastCheckpointID.String(), "the second commit's checkpoint is written too")
}
