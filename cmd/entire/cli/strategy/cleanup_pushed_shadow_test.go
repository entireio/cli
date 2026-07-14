package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// shadowCleanupEnv bundles the setup needed for testing post-push shadow
// branch cleanup: a git repo, a known base commit, and helpers to
// create shadow refs + matching session states.
type shadowCleanupEnv struct {
	t        *testing.T
	repo     *git.Repository
	dir      string
	baseHash plumbing.Hash
}

func newShadowCleanupEnv(t *testing.T) *shadowCleanupEnv {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	t.Chdir(dir)

	emptyTree := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	baseHash, err := checkpoint.CreateCommit(context.Background(), repo, emptyTree, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	require.NoError(t, err)
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))
	require.NoError(t, repo.Storer.SetReference(headRef))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), baseHash)))
	return &shadowCleanupEnv{t: t, repo: repo, dir: dir, baseHash: baseHash}
}

// addShadowBranch creates a shadow branch for the given (base, worktreeID)
// pair and returns its derived name.
func (e *shadowCleanupEnv) addShadowBranch(baseCommit, worktreeID string) string {
	e.t.Helper()
	name := getShadowBranchNameForCommit(baseCommit, worktreeID)
	require.NoError(e.t, e.repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), e.baseHash)))
	return name
}

// addSessionState writes a session state file. If ended is non-nil the
// session is treated as ended; pendingCheckpoints simulates the
// mid-finalize race window.
func (e *shadowCleanupEnv) addSessionState(sessionID, baseCommit, worktreeID string, ended *time.Time, pendingCheckpoints []string, fullyCondensed bool) {
	e.t.Helper()
	phase := session.PhaseActive
	if ended != nil {
		phase = session.PhaseEnded
	}
	state := &SessionState{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		WorktreeID:        worktreeID,
		StartedAt:         time.Now().Add(-time.Hour),
		EndedAt:           ended,
		Phase:             phase,
		FullyCondensed:    fullyCondensed,
		TurnCheckpointIDs: pendingCheckpoints,
	}
	require.NoError(e.t, SaveSessionState(context.Background(), state))
}

func (e *shadowCleanupEnv) branchExists(name string) bool {
	e.t.Helper()
	_, err := e.repo.Reference(plumbing.NewBranchReferenceName(name), false)
	return err == nil
}

// Predicate matrix: each shadow branch is paired with zero or more
// session states; the cleanup must respect the safety rules (active
// session OR pending turn checkpoints protect the branch; ended-clean
// or orphaned branches are deleted).
func TestCleanupPushedShadowBranches_Predicate(t *testing.T) {
	ended := time.Now().Add(-time.Minute)
	type sessionFixture struct {
		id                string
		ended             *time.Time
		pendingCheckpoint []string
		fullyCondensed    bool
	}
	cases := []struct {
		name        string
		sessions    []sessionFixture
		wantDeleted bool
	}{
		{name: "ended_fully_condensed_deleted", sessions: []sessionFixture{{id: "s1", ended: &ended, fullyCondensed: true}}, wantDeleted: true},
		{name: "ended_not_fully_condensed_preserved", sessions: []sessionFixture{{id: "s1", ended: &ended}}, wantDeleted: false},
		{name: "active_session_preserved", sessions: []sessionFixture{{id: "s1", ended: &ended, fullyCondensed: true}, {id: "s2", ended: nil}}, wantDeleted: false},
		{name: "pending_turn_checkpoints_preserved", sessions: []sessionFixture{{id: "s1", ended: &ended, pendingCheckpoint: []string{"a1b2c3d4e5f6"}}}, wantDeleted: false},
		{name: "orphaned_branch_no_sessions_deleted", sessions: nil, wantDeleted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newShadowCleanupEnv(t)
			shadow := env.addShadowBranch(env.baseHash.String(), "")
			for _, s := range tc.sessions {
				env.addSessionState(s.id, env.baseHash.String(), "", s.ended, s.pendingCheckpoint, s.fullyCondensed)
			}
			deleted, err := CleanupPushedShadowBranches(context.Background())
			require.NoError(t, err)
			if tc.wantDeleted {
				require.Equal(t, 1, deleted)
				require.False(t, env.branchExists(shadow))
			} else {
				require.Equal(t, 0, deleted)
				require.True(t, env.branchExists(shadow))
			}
		})
	}
}

// Mixed: two shadow branches with different worktree IDs and different
// session statuses. The cleanup must delete only the safe one.
func TestCleanupPushedShadowBranches_MixedBranchesPartialDelete(t *testing.T) {
	env := newShadowCleanupEnv(t)
	preserved := env.addShadowBranch(env.baseHash.String(), "wt1")
	deletable := env.addShadowBranch(env.baseHash.String(), "wt2")
	ended := time.Now().Add(-time.Minute)
	env.addSessionState("s-active", env.baseHash.String(), "wt1", nil, nil, false)
	env.addSessionState("s-ended", env.baseHash.String(), "wt2", &ended, nil, true)

	deleted, err := CleanupPushedShadowBranches(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
	require.True(t, env.branchExists(preserved))
	require.False(t, env.branchExists(deletable))
}

// No shadow branches → no-op, no error.
func TestCleanupPushedShadowBranches_NoBranches_NoOp(t *testing.T) {
	env := newShadowCleanupEnv(t)
	_ = env

	deleted, err := CleanupPushedShadowBranches(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, deleted)
}

func TestDeleteShadowBranchesIfUnchanged_PreservesMovedBranch(t *testing.T) {
	env := newShadowCleanupEnv(t)
	shadow := env.addShadowBranch(env.baseHash.String(), "")

	emptyTree := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	newHash, err := checkpoint.CreateCommit(context.Background(), env.repo, emptyTree, env.baseHash,
		"new checkpoint", "test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, env.repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadow), newHash)))

	deleted, failed := DeleteShadowBranchesIfUnchanged(context.Background(), map[string]plumbing.Hash{
		shadow: env.baseHash,
	})
	require.Empty(t, deleted)
	require.Equal(t, []string{shadow}, failed)
	require.True(t, env.branchExists(shadow))

	ref, err := env.repo.Reference(plumbing.NewBranchReferenceName(shadow), false)
	require.NoError(t, err)
	require.Equal(t, newHash, ref.Hash())
}

func TestDeleteShadowBranchesIfUnchanged_PreservesBranchProtectedAfterSnapshot(t *testing.T) {
	env := newShadowCleanupEnv(t)
	shadow := env.addShadowBranch(env.baseHash.String(), "")
	snapshot := map[string]plumbing.Hash{
		shadow: env.baseHash,
	}

	env.addSessionState("s-race", env.baseHash.String(), "", nil, nil, false)

	deleted, failed := DeleteShadowBranchesIfUnchanged(context.Background(), snapshot)
	require.Empty(t, deleted)
	require.Equal(t, []string{shadow}, failed)
	require.True(t, env.branchExists(shadow))
}
