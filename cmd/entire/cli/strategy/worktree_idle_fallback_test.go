package strategy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/session"
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
