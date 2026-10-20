package strategy

import (
	"context"
	"testing"
	"time"

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
// and the sweeper.
//
// Not parallel: uses t.Chdir()
func TestCondense_SubagentSessionHandsItsFilesToTheParent(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)

	for name, condense := range map[string]func(*ManualCommitStrategy, string) error{
		"eager": func(s *ManualCommitStrategy, id string) error { return s.CondenseAndMarkFullyCondensed(ctx, id) },
		"by id": func(s *ManualCommitStrategy, id string) error { return s.CondenseSessionByID(ctx, id) },
	} {
		t.Run(name, func(t *testing.T) {
			saveIdentitySession(t, "sess-parent", func(st *SessionState) {
				st.WorktreePath = dir
				st.FilesTouched = []string{"parent.txt"}
			})
			saveIdentitySession(t, "sess-subagent", func(st *SessionState) {
				st.WorktreePath = dir
				st.ParentSessionID = "sess-parent"
				st.FilesTouched = []string{"early.txt"}
			})

			require.NoError(t, condense(NewManualCommitStrategy(), "sess-subagent"))

			child, err := LoadSessionState(ctx, "sess-subagent")
			require.NoError(t, err)
			require.NotNil(t, child)
			assert.True(t, child.FullyCondensed)
			assert.False(t, child.HasPendingWork(), "nothing left for doctor or the sweeper to offer")
			parent, err := LoadSessionState(ctx, "sess-parent")
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"parent.txt", "early.txt"}, parent.FilesTouched)
		})
	}
}
