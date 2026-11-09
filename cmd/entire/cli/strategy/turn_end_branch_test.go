package strategy

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestHandleTurnEnd_RefreshesOnlyHomeBranch(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	mainDir, linkedDir := twoWorktrees(t)
	testutil.RunGit(t, mainDir, "checkout", "-b", "home-at-turn-end")
	unavailable := resolvedTempDir(t)
	for _, tc := range []struct {
		name     string
		root     string
		home     string
		detached bool
		want     string
	}{
		{name: "home", root: mainDir, home: mainDir, want: "home-at-turn-end"},
		{name: "foreign hook", root: linkedDir, home: mainDir, want: "recorded-at-turn-start"},
		{name: "unavailable hook root", root: unavailable, home: mainDir, want: "recorded-at-turn-start"},
		{name: "removed home", root: linkedDir, home: filepath.Join(unavailable, "removed"), want: "recorded-at-turn-start"},
		{name: "detached home", root: mainDir, home: mainDir, detached: true},
	} {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // uses t.Chdir
			chdirWorktree(t, tc.root)
			if tc.detached {
				testutil.RunGit(t, tc.root, "checkout", "--detach")
			}
			state := &SessionState{WorktreePath: tc.home, Branch: "recorded-at-turn-start"}
			require.NoError(t, (&ManualCommitStrategy{}).HandleTurnEnd(t.Context(), state, time.Time{}))
			assert.Equal(t, tc.want, state.Branch)
		})
	}
}
