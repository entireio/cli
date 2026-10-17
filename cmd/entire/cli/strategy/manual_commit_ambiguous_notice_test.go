package strategy

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestFormatAmbiguousWorktreeNotice verifies the user-facing stderr message: it
// always reports the ambiguity, and when an adoptable session is available it
// appends a directly-runnable adopt command (concrete session ID so it doesn't
// depend on adoption's 12h auto-detect, plus --yes for same-repo adoption).
func TestFormatAmbiguousWorktreeNotice(t *testing.T) {
	t.Parallel()

	t.Run("with adoptable session includes runnable command", func(t *testing.T) {
		t.Parallel()
		primary := &SessionState{SessionID: "sess-1", WorktreePath: "/repo/a"}
		got := formatAmbiguousWorktreeNotice([]string{"/repo/a", "/repo/b"}, primary)
		require.Contains(t, got, "not linked to a checkpoint")
		require.Contains(t, got, "/repo/a, /repo/b")
		require.Contains(t, got, "entire session adopt sess-1 --from '/repo/a' --yes")
	})

	t.Run("worktree path with spaces is shell-quoted", func(t *testing.T) {
		t.Parallel()
		primary := &SessionState{SessionID: "sess-1", WorktreePath: "/Users/cole/my repo"}
		got := formatAmbiguousWorktreeNotice([]string{"/Users/cole/my repo"}, primary)
		require.Contains(t, got, "--from '/Users/cole/my repo' --yes")
	})

	t.Run("without a session still reports ambiguity but no command", func(t *testing.T) {
		t.Parallel()
		got := formatAmbiguousWorktreeNotice([]string{"/repo/a", "/repo/b"}, nil)
		require.Contains(t, got, "/repo/a, /repo/b")
		require.NotContains(t, got, "entire session adopt")
	})

	t.Run("no worktrees yields no message", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, formatAmbiguousWorktreeNotice(nil, &SessionState{SessionID: "x", WorktreePath: "/repo/a"}))
	})
}

// TestShellSingleQuote covers pasteability of paths with spaces and embedded
// single quotes.
func TestShellSingleQuote(t *testing.T) {
	t.Parallel()

	require.Equal(t, "'/repo/a'", shellSingleQuote("/repo/a"))
	require.Equal(t, "'/repo/my dir'", shellSingleQuote("/repo/my dir"))
	require.Equal(t, `'/repo/it'\''s'`, shellSingleQuote("/repo/it's"))
}

// TestMostRecentlyAdoptableSession verifies the source named in the remedy is
// the newest candidate adoption would actually accept.
func TestMostRecentlyAdoptableSession(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-30 * time.Minute)
	newer := time.Now().Add(-1 * time.Minute)
	ended := time.Now()

	t.Run("picks newest adoptable", func(t *testing.T) {
		t.Parallel()
		got := mostRecentlyAdoptableSession([]*SessionState{
			{SessionID: "old", WorktreePath: "/a", LastInteractionTime: &older},
			{SessionID: "new", WorktreePath: "/b", LastInteractionTime: &newer},
		})
		require.NotNil(t, got)
		require.Equal(t, "new", got.SessionID)
	})

	t.Run("skips ended, ended-at, and fully-condensed sessions", func(t *testing.T) {
		t.Parallel()
		got := mostRecentlyAdoptableSession([]*SessionState{
			{SessionID: "ended", WorktreePath: "/a", Phase: session.PhaseEnded, LastInteractionTime: &newer},
			{SessionID: "condensed", WorktreePath: "/b", FullyCondensed: true, LastInteractionTime: &newer},
			{SessionID: "endedat", WorktreePath: "/c", EndedAt: &ended, LastInteractionTime: &newer},
			{SessionID: "ok", WorktreePath: "/d", LastInteractionTime: &older},
		})
		require.NotNil(t, got)
		require.Equal(t, "ok", got.SessionID)
	})

	t.Run("nil when none adoptable", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, mostRecentlyAdoptableSession([]*SessionState{{SessionID: "ended", Phase: session.PhaseEnded}}))
		require.Nil(t, mostRecentlyAdoptableSession(nil))
	})
}
