package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

// saveSiblingSession saves a session homed in dir with the given phase, last
// interaction and task records.
func saveSiblingSession(t *testing.T, s *ManualCommitStrategy, dir, id string, phase session.Phase, last *time.Time, tasks ...session.TaskRecord) {
	t.Helper()
	ctx := context.Background()
	state := &SessionState{SessionID: id, WorktreePath: dir, LastInteractionTime: last, TaskRecords: tasks}
	saveSessionMatchState(ctx, t, s, dir, state)
	state.Phase = phase
	require.NoError(t, s.saveSessionState(ctx, state))
}

// A commit from a worktree with no session of its own falls back to the
// sessions of a sibling worktree. One whose last interaction is older than
// recentSessionWindow is not plausibly the commit's session, so it is not
// linked: post-commit would otherwise condense its whole transcript into every
// commit made here (cli issue 2979). Kept: a session with no recorded
// interaction, as before, one mid-turn whose turn started within
// activeSessionInteractionThreshold, and one whose background subagent is
// still running.
func TestCommitLinking_FallbackSkipsSiblingSessionsIdleBeyondTheWindow(t *testing.T) {
	now := time.Now()
	twoDaysAgo := now.Add(-48 * time.Hour)
	dayAgo := now.Add(-24 * time.Hour)
	justPast := now.Add(-recentSessionWindow - time.Minute)
	recent := now.Add(-time.Minute)
	type sess struct {
		phase session.Phase
		last  *time.Time
		tasks []session.TaskRecord
	}
	inFlight := []session.TaskRecord{{ToolUseID: "call_bg", AgentID: "child", StartedAt: justPast}}
	finished := []session.TaskRecord{{ToolUseID: "call_bg", AgentID: "child", StartedAt: dayAgo, CompletedAt: justPast}}
	for _, tt := range []struct {
		name     string
		sessions map[string]sess
		want     []string
	}{
		{name: "idle for a day", sessions: map[string]sess{"stale": {session.PhaseIdle, &dayAgo, nil}}},
		{name: "idle just past the window", sessions: map[string]sess{"stale": {session.PhaseIdle, &justPast, nil}}},
		{name: "recent", sessions: map[string]sess{"recent": {session.PhaseIdle, &recent, nil}}, want: []string{"recent"}},
		{name: "no recorded interaction", sessions: map[string]sess{"undated": {session.PhaseIdle, nil, nil}}, want: []string{"undated"}},
		{name: "mid-turn past the window", sessions: map[string]sess{"long-turn": {session.PhaseActive, &justPast, nil}}, want: []string{"long-turn"}},
		{name: "left active for two days", sessions: map[string]sess{"abandoned": {session.PhaseActive, &twoDaysAgo, nil}}},
		{name: "idle with a subagent in flight", sessions: map[string]sess{"bg": {session.PhaseIdle, &dayAgo, inFlight}}, want: []string{"bg"}},
		{name: "idle with a finished subagent", sessions: map[string]sess{"bg": {session.PhaseIdle, &dayAgo, finished}}},
		{
			name: "stale beside recent and undated",
			sessions: map[string]sess{
				"stale":   {session.PhaseIdle, &dayAgo, nil},
				"recent":  {session.PhaseIdle, &recent, nil},
				"undated": {session.PhaseIdle, nil, nil},
			},
			want: []string{"recent", "undated"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mainDir, linkedDir := twoWorktrees(t)
			s := &ManualCommitStrategy{}
			for id, st := range tt.sessions {
				saveSiblingSession(t, s, mainDir, id, st.phase, st.last, st.tasks...)
			}
			assert.ElementsMatch(t, tt.want, linkingIDs(t, linkedDir))
		})
	}
}

// An idle sibling session that recorded edits in the committing worktree still
// joins the commit, held to those edits: the idle check only narrows the
// fallback, not the sessions that edited this worktree from elsewhere.
func TestCommitLinking_IdleSiblingSessionThatEditedHereStillJoins(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:           "edited-here",
		WorktreePath:        mainDir,
		LastInteractionTime: &dayAgo,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"feature.go"}, Incarnation: mark(t, linkedDir)},
		},
	})

	linked := linkingSessions(t, linkedDir)
	require.Contains(t, linked, "edited-here")
	assert.True(t, linked["edited-here"].LinkedByWorktreeEntry, "held to the edits it recorded here")
}

// A session prepare-commit-msg linked is kept by the idle check however idle
// it has become since; the other fallback sessions are still judged.
func TestCommitLinking_FallbackKeepsTheSessionsPrepareLinked(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	saveSiblingSession(t, s, mainDir, "linked", session.PhaseIdle, &dayAgo)
	saveSiblingSession(t, s, mainDir, "stale", session.PhaseIdle, &dayAgo)

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	sessions, err := s.findSessionsForCommitLinking(withLinkedSessions(context.Background(), []string{"linked"}), linkedDir)
	require.NoError(t, err)
	assert.Equal(t, []string{"linked"}, linkedSessionIDs(sessions))
}

// Prepare links a sibling worktree's session that is not idle when it runs,
// and stamps the trailer. The commit can then wait on the TTY prompt or the
// message editor, and by post-commit the session looks idle: past
// recentSessionWindow, or with its background subagent finished. Post-commit
// keeps the sessions prepare recorded for the trailer, so it still condenses
// that session into the stamped checkpoint. Judged afresh, it dropped the
// session and left the trailer naming a checkpoint that is never written.
func TestCommitHooks_LinkedSiblingSessionCondensesAfterALongWait(t *testing.T) {
	now := time.Now()
	recent := now.Add(-recentSessionWindow + time.Minute)
	justPast := now.Add(-recentSessionWindow - time.Minute)
	dayAgo := now.Add(-24 * time.Hour)
	for _, tt := range []struct {
		name             string
		atPrepare        func(*SessionState)
		beforePostCommit func(*SessionState)
	}{
		{
			name:             "aged past the window",
			atPrepare:        func(st *SessionState) { st.LastInteractionTime = &recent },
			beforePostCommit: func(st *SessionState) { st.LastInteractionTime = &justPast },
		},
		{
			name: "its subagent finished",
			atPrepare: func(st *SessionState) {
				st.LastInteractionTime = &dayAgo
				st.TaskRecords = []session.TaskRecord{{ToolUseID: "call_bg", AgentID: "child", StartedAt: justPast}}
			},
			beforePostCommit: func(st *SessionState) { st.TaskRecords[0].CompletedAt = time.Now() },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			homeDir, err := filepath.EvalSymlinks(setupGitRepo(t))
			require.NoError(t, err, "the recorded home must match git's worktree list")
			t.Chdir(homeDir)
			s := NewManualCommitStrategy()
			setupSessionWithCheckpointAndFile(t, s, homeDir, "sess-sibling", "notes.txt")
			updateSession := func(change func(*SessionState)) *SessionState {
				t.Helper()
				state, err := s.loadSessionState(ctx, "sess-sibling")
				require.NoError(t, err)
				state.WorktreePath = homeDir
				state.Phase = session.PhaseIdle
				change(state)
				require.NoError(t, s.saveSessionState(ctx, state))
				return state
			}
			updateSession(tt.atPrepare)

			siblingDir := addSiblingWorktree(t, homeDir, "commit-wt")
			t.Chdir(siblingDir)
			testutil.WriteFile(t, siblingDir, "notes.txt", "agent content for notes.txt")
			testutil.GitAdd(t, siblingDir, "notes.txt")
			msg := prepareMessage(t, "add notes\n")
			cps := checkpointIDs(msg)
			require.Len(t, cps, 1, "prepare links the session, not idle when it runs: %q", msg)

			state := updateSession(tt.beforePostCommit)
			require.True(t, idleBeyondRecentWindow(state, time.Now()), "sanity: by post-commit the session looks idle")
			testutil.GitCommit(t, siblingDir, msg)
			require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))

			state, err = s.loadSessionState(ctx, "sess-sibling")
			require.NoError(t, err)
			assert.Equal(t, cps[0], state.LastCheckpointID.String(), "post-commit condenses the session prepare linked")
		})
	}
}

// Post-commit reads the sessions prepare recorded for the trailer it condenses
// into, and the ones among them it trusted on their shape alone, and consumes
// the record with the stamp record.
func TestStampedSessions_RecordedWithTheTrailerAndTakenOnce(t *testing.T) {
	t.Chdir(setupGitRepo(t))
	reserved, other := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")
	require.True(t, recordStampedTrailer(withLinkedSessions(context.Background(), []string{"first"}), reserved))
	later := withTrustedSessions(withLinkedSessions(context.Background(), []string{"second", "third"}), []string{"third"})
	require.True(t, recordStampedTrailer(later, reserved), "a later prepare stamping the same reservation")
	require.True(t, recordStampedTrailer(context.Background(), other), "a stamp with no linked sessions records none")

	assert.Equal(t, map[id.CheckpointID]stampedSessions{
		reserved: {CheckpointID: reserved, SessionIDs: []string{"second", "third"}, TrustedSessionIDs: []string{"third"}},
	}, takeStampedSessions(context.Background()))
	assert.Empty(t, takeStampedSessions(context.Background()), "post-commit consumes the record")
}

// The idle check is for linking new commits only. Post-rewrite in the
// worktree still finds an idle sibling session and remaps its BaseCommit, as
// it keeps an existing link rather than making a new one.
func TestPostRewrite_StillRemapsAnIdleSiblingSession(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	saveSiblingSession(t, s, mainDir, "idle", session.PhaseIdle, &dayAgo)
	before, err := s.loadSessionState(ctx, "idle")
	require.NoError(t, err)
	newSHA := strings.Repeat("b", 40)

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	require.NoError(t, s.PostRewrite(ctx, "amend", strings.NewReader(before.BaseCommit+" "+newSHA+"\n")))

	after, err := s.loadSessionState(ctx, "idle")
	require.NoError(t, err)
	assert.Equal(t, newSHA, after.BaseCommit)
}
