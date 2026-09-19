package strategy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// rehomeFixture: hook CWD in a linked worktree, session homed in the main checkout.
type rehomeFixture struct {
	mainDir     string
	worktreeDir string
	head        string
	state       *SessionState
}

func newRehomeFixture(t *testing.T) rehomeFixture {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	mainDir := setupSessionMatchRepo(t)
	worktreeDir := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, worktreeDir, "feature")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, worktreeDir) })
	t.Chdir(worktreeDir)
	clearSessionMatchCaches()
	return rehomeFixture{
		mainDir:     mainDir,
		worktreeDir: worktreeDir,
		head:        testutil.GetHeadHash(t, worktreeDir),
		state: &SessionState{
			SessionID:             "parent-homed",
			WorktreePath:          mainDir,
			BaseCommit:            "0000000000000000000000000000000000000000",
			AttributionBaseCommit: "0000000000000000000000000000000000000000",
			Phase:                 session.PhaseActive,
		},
	}
}

func TestRehomeSessionAfterOwnCommit_MovesParentHomedSessionIntoWorktree(t *testing.T) {
	fx := newRehomeFixture(t)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	require.True(t, s.rehomeSessionAfterOwnCommit(ctx, repo, fx.state, fx.worktreeDir, fx.head, true, fx.state.SessionID))

	wantID, err := paths.GetWorktreeID(fx.worktreeDir)
	require.NoError(t, err)
	assert.Equal(t, fx.worktreeDir, fx.state.WorktreePath)
	assert.Equal(t, wantID, fx.state.WorktreeID, "WorktreeID must move with WorktreePath")
	assert.Equal(t, fx.head, fx.state.BaseCommit)
	assert.Equal(t, fx.head, fx.state.AttributionBaseCommit)
	assert.Equal(t, "feature", fx.state.Branch)
}

func TestRehomeSessionAfterOwnCommit_KeepsHomeWhenPendingContentIsThere(t *testing.T) {
	fx := newRehomeFixture(t)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	for name, pending := range map[string]func(*SessionState){
		"tracked files": func(st *SessionState) { st.FilesTouched = []string{"home.go"} },
		"shadow steps":  func(st *SessionState) { st.StepCount = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			state := *fx.state
			pending(&state)
			s := &ManualCommitStrategy{}
			require.False(t, s.rehomeSessionAfterOwnCommit(ctx, repo, &state, fx.worktreeDir, fx.head, true, state.SessionID),
				"a home that still holds pending content must not be abandoned")
			assert.Equal(t, fx.mainDir, state.WorktreePath)
			assert.Equal(t, fx.state.BaseCommit, state.BaseCommit)
		})
	}
}

func TestRehomeSessionAfterOwnCommit_OnlyForTheCondensedAncestryGuest(t *testing.T) {
	fx := newRehomeFixture(t)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	cases := map[string]struct {
		condensed bool
		guest     string
		worktree  string
	}{
		"not condensed by this commit":   {condensed: false, guest: fx.state.SessionID, worktree: fx.worktreeDir},
		"reached the set by path only":   {condensed: true, guest: "", worktree: fx.worktreeDir},
		"ancestry named another session": {condensed: true, guest: "someone-else", worktree: fx.worktreeDir},
		"commit in the home worktree":    {condensed: true, guest: fx.state.SessionID, worktree: fx.mainDir},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			state := *fx.state
			require.False(t, s.rehomeSessionAfterOwnCommit(ctx, repo, &state, tc.worktree, fx.head, tc.condensed, tc.guest))
			assert.Equal(t, fx.mainDir, state.WorktreePath)
		})
	}
}
