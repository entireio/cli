package strategy

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestIsLiveSessionInOtherWorktree covers the decision that gates the #1852
// warning: a commit with no matching session should only warn when a still-open,
// adoptable, recently-active agent session lives in a *different* worktree of the
// same repo.
func TestIsLiveSessionInOtherWorktree(t *testing.T) {
	t.Parallel()

	const here = "/repo/main"
	const sibling = "/repo/feature"

	recent := time.Now().Add(-1 * time.Minute)
	// Older than activeSessionInteractionThreshold (24h): a crashed/abandoned
	// session that never reached ENDED.
	stale := time.Now().Add(-72 * time.Hour)
	ended := time.Now().Add(-2 * time.Minute)

	tests := []struct {
		name  string
		state *SessionState
		want  bool
	}{
		{name: "nil state", state: nil, want: false},
		{name: "no worktree path", state: &SessionState{Phase: session.PhaseActive, LastInteractionTime: &recent}, want: false},
		{name: "same worktree", state: &SessionState{WorktreePath: here, Phase: session.PhaseActive, LastInteractionTime: &recent}, want: false},
		{name: "sibling active and recent", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive, LastInteractionTime: &recent}, want: true},
		{name: "sibling idle and recent is still live", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseIdle, LastInteractionTime: &recent}, want: true},
		{name: "sibling active but stale", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive, LastInteractionTime: &stale}, want: false},
		{name: "sibling active but never interacted", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive}, want: false},
		{name: "sibling ended by phase", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseEnded, LastInteractionTime: &recent}, want: false},
		{name: "sibling ended by EndedAt", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive, LastInteractionTime: &recent, EndedAt: &ended}, want: false},
		{name: "sibling fully condensed", state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive, LastInteractionTime: &recent, FullyCondensed: true}, want: false},
		{
			name:  "sibling adopted-away tombstone",
			state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive, LastInteractionTime: &recent, AdoptedIntoWorktreePath: here},
			want:  false,
		},
		{
			name:  "sibling imported session",
			state: &SessionState{WorktreePath: sibling, Phase: session.PhaseActive, LastInteractionTime: &recent, Kind: session.KindImported},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isLiveSessionInOtherWorktree(tt.state, here))
		})
	}
}

// TestMostRecentlyActiveSession verifies the source session named in the remedy
// is the newest by last-seen time (so it matches adoption's own auto-selection).
func TestMostRecentlyActiveSession(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-30 * time.Minute)
	newer := time.Now().Add(-1 * time.Minute)

	states := []*SessionState{
		{SessionID: "old", LastInteractionTime: &older},
		{SessionID: "new", LastInteractionTime: &newer},
	}

	require.Equal(t, "new", mostRecentlyActiveSession(states).SessionID)
	require.Nil(t, mostRecentlyActiveSession(nil))
}

// TestUniqueWorktreePaths verifies distinct paths are returned in first-seen
// order for compact warning output.
func TestUniqueWorktreePaths(t *testing.T) {
	t.Parallel()

	states := []*SessionState{
		{WorktreePath: "/repo/a"},
		{WorktreePath: "/repo/b"},
		{WorktreePath: "/repo/a"},
		{WorktreePath: "/repo/c"},
		{WorktreePath: "/repo/b"},
	}

	require.Equal(t, []string{"/repo/a", "/repo/b", "/repo/c"}, uniqueWorktreePaths(states))
}

// TestUniqueWorktreePaths_Empty ensures no paths are returned for no input.
func TestUniqueWorktreePaths_Empty(t *testing.T) {
	t.Parallel()

	require.Empty(t, uniqueWorktreePaths(nil))
}

// TestFormatUnlinkedCommitNotice verifies the user-facing stderr message names
// the worktree(s) and emits a directly-runnable adopt command: the concrete
// session ID (so it doesn't depend on adoption's 12h auto-detect window) plus
// --yes (required for same-repo adoption).
func TestFormatUnlinkedCommitNotice(t *testing.T) {
	t.Parallel()

	t.Run("names the session and worktree", func(t *testing.T) {
		t.Parallel()
		got := formatUnlinkedCommitNotice([]string{"/repo/feature"}, "sess-123", "/repo/feature")
		require.Contains(t, got, "not linked to a checkpoint")
		require.Contains(t, got, "entire session adopt sess-123 --from /repo/feature --yes")
	})

	t.Run("lists multiple worktrees for context", func(t *testing.T) {
		t.Parallel()
		got := formatUnlinkedCommitNotice([]string{"/repo/a", "/repo/b"}, "sess-9", "/repo/a")
		require.Contains(t, got, "/repo/a, /repo/b")
		require.Contains(t, got, "adopt sess-9 --from /repo/a --yes")
	})

	t.Run("missing components yield no message", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, formatUnlinkedCommitNotice(nil, "sess-1", "/repo/a"))
		require.Empty(t, formatUnlinkedCommitNotice([]string{"/repo/a"}, "", "/repo/a"))
		require.Empty(t, formatUnlinkedCommitNotice([]string{"/repo/a"}, "sess-1", ""))
	})
}
