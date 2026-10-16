package strategy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// Files waiting in another worktree belong to that worktree's commit, as the
// home's do to the home's: eager condensation and the session-end path leave
// them alone instead of marking the session condensed.
func TestPendingElsewhereIsNotCondensedEagerly(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:      "elsewhere",
		Phase:          session.PhaseEnded,
		OtherWorktrees: map[string]*session.WorktreeWork{"wt": {FilesTouched: []string{"a.go"}}},
	}
	_, err := prepareEagerCondensation(state)
	require.ErrorIs(t, err, ErrMutationSkip)
	assert.False(t, state.FullyCondensed)

	require.NoError(t, PrepareSessionEndCondensation(t.Context(), state))
	assert.False(t, state.FullyCondensed, "an ENDED session with files elsewhere must stay eligible for their commit")
}
