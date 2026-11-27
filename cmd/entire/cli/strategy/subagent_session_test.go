package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A subagent run as a session of its own (agy's subagents, Droid's Workers)
// can share its parent's agent process, so identity matching ties the two and
// recency would pick the subagent. Once marked, it never links a commit.
//
// Not parallel: uses t.Chdir()
func TestFindSessionsForCommitLinking_SubagentSessionNeverLinks(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)
	anc := selfAncestorOwner(t)
	earlier := time.Now().Add(-time.Minute)

	saveIdentitySession(t, "sess-parent", func(st *SessionState) {
		st.Owner = anc
		st.WorktreePath = dir
		st.LastInteractionTime = &earlier
	})
	saveIdentitySession(t, "sess-subagent", func(st *SessionState) {
		st.Owner = anc
		st.WorktreePath = dir
		st.ParentSessionID = "sess-parent"
	})

	got, err := NewManualCommitStrategy().findSessionsForCommitLinking(ctx, dir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "sess-parent", got[0].SessionID)
}

// Not parallel: uses t.Chdir() and t.Setenv()
func TestResolveCallerSession_NeverAnswersWithASubagentSession(t *testing.T) {
	clearCallerSessionEnv(t)
	dir := callerSessionRepo(t)
	anc := selfAncestorOwner(t)
	earlier := time.Now().Add(-time.Minute)

	saveIdentitySession(t, "sess-parent", func(st *SessionState) {
		st.Owner = anc
		st.WorktreePath = dir
		st.LastInteractionTime = &earlier
	})
	saveIdentitySession(t, "sess-subagent", func(st *SessionState) {
		st.Owner = anc
		st.WorktreePath = dir
		st.ParentSessionID = "sess-parent"
	})

	got := ResolveCallerSession(context.Background())
	assert.Equal(t, "sess-parent", got.SessionID, "resolution %s", got.Resolution)
}

// Condensing a subagent's own session means handing what it holds to its
// parent: it never checkpoints alone, and must stop looking stuck to doctor
// and the sweeper. Condensing it by ID (doctor's fix for a stuck session) also
// retires an ACTIVE subagent to IDLE, as it does a top-level session; an ENDED
// one stays ENDED, and the eager condense at session end leaves the phase to
// the turn that owns it.
//
// Not parallel: uses t.Chdir()
func TestCondense_SubagentSessionHandsItsFilesToTheParent(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)
	eager := func(s *ManualCommitStrategy, id string) error { return s.CondenseAndMarkFullyCondensed(ctx, id) }
	byID := func(s *ManualCommitStrategy, id string) error { return s.CondenseSessionByID(ctx, id) }

	for name, tc := range map[string]struct {
		condense  func(*ManualCommitStrategy, string) error
		phase     session.Phase
		wantPhase session.Phase
	}{
		"eager":        {condense: eager, phase: session.PhaseActive, wantPhase: session.PhaseActive},
		"by id, stuck": {condense: byID, phase: session.PhaseActive, wantPhase: session.PhaseIdle},
		"by id, idle":  {condense: byID, phase: session.PhaseIdle, wantPhase: session.PhaseIdle},
		"by id, ended": {condense: byID, phase: session.PhaseEnded, wantPhase: session.PhaseEnded},
	} {
		t.Run(name, func(t *testing.T) {
			saveIdentitySession(t, "sess-parent", func(st *SessionState) {
				st.WorktreePath = dir
				st.FilesTouched = []string{"parent.txt"}
			})
			longAgo := time.Now().Add(-2 * time.Hour)
			saveIdentitySession(t, "sess-subagent", func(st *SessionState) {
				st.WorktreePath = dir
				st.ParentSessionID = "sess-parent"
				st.FilesTouched = []string{"early.txt"}
				st.Phase = tc.phase
				st.LastInteractionTime = &longAgo
			})

			require.NoError(t, tc.condense(NewManualCommitStrategy(), "sess-subagent"))

			child, err := LoadSessionState(ctx, "sess-subagent")
			require.NoError(t, err)
			require.NotNil(t, child)
			assert.True(t, child.FullyCondensed)
			assert.False(t, child.HasPendingWork(), "nothing left for doctor or the sweeper to offer")
			assert.Equal(t, tc.wantPhase, child.Phase)
			parent, err := LoadSessionState(ctx, "sess-parent")
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"parent.txt", "early.txt"}, parent.FilesTouched)
		})
	}
}

// A handoff that cannot reach the parent leaves the files on the subagent, so
// a later condense can retry it.
//
// Not parallel: uses t.Chdir()
func TestCondense_SubagentSessionKeepsItsFilesWithoutAParent(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)
	saveIdentitySession(t, "sess-orphan-subagent", func(st *SessionState) {
		st.WorktreePath = dir
		st.ParentSessionID = "sess-missing-parent"
		st.FilesTouched = []string{"early.txt"}
	})

	require.Error(t, NewManualCommitStrategy().CondenseSessionByID(ctx, "sess-orphan-subagent"))

	child, err := LoadSessionState(ctx, "sess-orphan-subagent")
	require.NoError(t, err)
	assert.Equal(t, []string{"early.txt"}, child.FilesTouched)
	assert.False(t, child.FullyCondensed)
}
