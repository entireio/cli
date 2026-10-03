package strategy

import (
	"context"
	"path/filepath"
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

func TestRehomeSessionToCurrentWorktree_NeedsAStrongSignal(t *testing.T) {
	fx := newRehomeFixture(t)
	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	cases := map[string]struct {
		ctx        context.Context
		editedHere bool
		moved      bool
	}{
		"hook merely runs here":    {ctx: context.Background()},
		"payload named this tree":  {ctx: WithAgentWorkingTree(context.Background()), moved: true},
		"hook captured edits here": {ctx: context.Background(), editedHere: true, moved: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			state := *fx.state
			s.rehomeSessionToCurrentWorktree(tc.ctx, repo, &state, tc.editedHere)
			if tc.moved {
				assert.Equal(t, fx.worktreeDir, state.WorktreePath)
				assert.Equal(t, fx.head, state.BaseCommit)
			} else {
				assert.Equal(t, fx.mainDir, state.WorktreePath, "a signal-less hook must not move the session")
			}
		})
	}
}

// The re-home invariant, for every hook-time entry point and every shape of
// pending work: a session moves only when nothing pending belongs to its old
// home, and a move never drops, rewrites or relocates the pending work
// itself. Moving with work left behind would orphan it; moving it along
// would hand it to the wrong tree.
func TestRehome_NeverOrphansOrRewritesPendingWork(t *testing.T) {
	fx := newRehomeFixture(t)
	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()
	s := &ManualCommitStrategy{}
	signalled := WithAgentWorkingTree(context.Background())

	entryPoints := map[string]func(*SessionState) bool{
		"own commit": func(st *SessionState) bool {
			return s.rehomeSessionAfterOwnCommit(signalled, repo, st, fx.worktreeDir, fx.head, true, st.SessionID)
		},
		"turn boundary": func(st *SessionState) bool {
			before := st.WorktreePath
			s.rehomeSessionToCurrentWorktree(signalled, repo, st, false)
			return st.WorktreePath != before
		},
		"turn end without a step": func(st *SessionState) bool {
			before := st.WorktreePath
			s.rehomeSessionAtTurnEnd(signalled, st)
			return st.WorktreePath != before
		},
	}
	task := session.TaskRecord{ToolUseID: "toolu_1", Files: []string{"t.go"}}
	shapes := map[string]struct {
		pending func(*SessionState)
		mayMove bool
	}{
		"nothing pending":                {pending: func(*SessionState) {}, mayMove: true},
		"files recorded in the new tree": {pending: func(st *SessionState) { st.FilesTouched = []string{"a.go"}; st.PendingContentWorktree = fx.worktreeDir }, mayMove: true},
		"a task recorded in the new tree": {pending: func(st *SessionState) {
			st.TaskRecords = []session.TaskRecord{task}
			st.PendingContentWorktree = fx.worktreeDir
		}, mayMove: true},
		"files recorded at home": {pending: func(st *SessionState) { st.FilesTouched = []string{"a.go"}; st.PendingContentWorktree = fx.mainDir }},
		"a task recorded at home": {pending: func(st *SessionState) {
			st.TaskRecords = []session.TaskRecord{task}
			st.PendingContentWorktree = fx.mainDir
		}},
		"files recorded in several trees": {pending: func(st *SessionState) {
			st.FilesTouched = []string{"a.go"}
			st.PendingContentWorktree = session.PendingContentInSeveralWorktrees
		}},
		"files from before locations existed": {pending: func(st *SessionState) { st.FilesTouched = []string{"a.go"} }},
		"shadow steps, even with files located here": {pending: func(st *SessionState) {
			st.StepCount = 1
			st.FilesTouched = []string{"a.go"}
			st.PendingContentWorktree = fx.worktreeDir
		}},
	}
	for entryName, rehome := range entryPoints {
		for shapeName, shape := range shapes {
			t.Run(entryName+"/"+shapeName, func(t *testing.T) {
				state := *fx.state
				shape.pending(&state)
				before := state
				before.FilesTouched = append([]string(nil), state.FilesTouched...)
				before.TaskRecords = append([]session.TaskRecord(nil), state.TaskRecords...)

				moved := rehome(&state)

				if !shape.mayMove {
					require.False(t, moved, "pending work belongs to the old home; moving would orphan it")
					assert.Equal(t, before.WorktreePath, state.WorktreePath)
					assert.Equal(t, before.WorktreeID, state.WorktreeID)
					assert.Equal(t, before.BaseCommit, state.BaseCommit)
				} else {
					require.True(t, moved, "nothing pending ties the session to its old home")
					assert.Equal(t, fx.worktreeDir, state.WorktreePath)
					wantID, err := paths.GetWorktreeID(fx.worktreeDir)
					require.NoError(t, err)
					assert.Equal(t, wantID, state.WorktreeID, "WorktreeID moves with WorktreePath")
					assert.Equal(t, fx.head, state.BaseCommit, "the base is re-derived in the new tree")
				}
				assert.Equal(t, before.FilesTouched, state.FilesTouched, "a re-home never drops or rewrites pending files")
				assert.Equal(t, before.TaskRecords, state.TaskRecords, "a re-home never drops or rewrites task records")
				assert.Equal(t, before.StepCount, state.StepCount, "a re-home never touches shadow steps")
				assert.Equal(t, before.PendingContentWorktree, state.PendingContentWorktree, "a re-home never relocates where pending work was recorded")
			})
		}
	}
}

// A home worktree that was removed (a disposable agent worktree, cleaned up
// after its work merged) cannot hold anything committable, so files and task
// records recorded there must not strand the session: every entry point moves
// it on a strong signal, and the pending work itself is left as it was.
// Shadow-branch steps still hold it: the shadow branch is keyed to the home's
// BaseCommit and WorktreeID, and re-homing does not move the ref.
func TestRehome_LeavesARemovedHomeEvenWithPendingWork(t *testing.T) {
	fx := newRehomeFixture(t)
	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()
	s := &ManualCommitStrategy{}
	signalled := WithAgentWorkingTree(context.Background())
	removedHome := filepath.Join(t.TempDir(), "removed-worktree")

	entryPoints := map[string]func(*SessionState) bool{
		"own commit": func(st *SessionState) bool {
			return s.rehomeSessionAfterOwnCommit(signalled, repo, st, fx.worktreeDir, fx.head, true, st.SessionID)
		},
		"turn boundary": func(st *SessionState) bool {
			s.rehomeSessionToCurrentWorktree(signalled, repo, st, false)
			return st.WorktreePath == fx.worktreeDir
		},
	}
	for name, rehome := range entryPoints {
		t.Run(name+"/files and tasks", func(t *testing.T) {
			state := *fx.state
			state.WorktreePath = removedHome
			state.FilesTouched = []string{"a.go"}
			state.TaskRecords = []session.TaskRecord{{ToolUseID: "toolu_1"}}
			state.PendingContentWorktree = removedHome

			require.True(t, rehome(&state), "a removed home must not strand the session")
			assert.Equal(t, fx.worktreeDir, state.WorktreePath)
			assert.Equal(t, []string{"a.go"}, state.FilesTouched, "the pending work is left as it was")
			assert.Len(t, state.TaskRecords, 1)
		})
		t.Run(name+"/shadow steps", func(t *testing.T) {
			state := *fx.state
			state.WorktreePath = removedHome
			state.StepCount = 1

			require.False(t, rehome(&state), "moving would lose track of the shadow branch keyed to the old home")
			assert.Equal(t, removedHome, state.WorktreePath)
			assert.Equal(t, fx.state.BaseCommit, state.BaseCommit)
		})
	}
}

// A payload naming the tree it named last time is no evidence the agent moved:
// Claude Code resets its shell's working directory after each command, so an
// agent working in a worktree through `cd` keeps reporting the launch checkout.
// Its own commit re-homed it into the worktree; the next turn boundary, still
// reporting the launch checkout, must not pull it back. A payload that changed
// is a move.
func TestRehomeSessionToCurrentWorktree_RepeatedPayloadIsNotAMove(t *testing.T) {
	fx := newRehomeFixture(t) // the hook runs in fx.worktreeDir
	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()
	s := &ManualCommitStrategy{}
	signalled := WithAgentWorkingTree(context.Background())

	cases := map[string]struct {
		lastReported string
		moved        bool
	}{
		"payload unchanged since the session moved away": {lastReported: fx.worktreeDir, moved: false},
		"payload changed: the agent moved":               {lastReported: fx.mainDir, moved: true},
		"nothing recorded yet":                           {lastReported: "", moved: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			state := *fx.state // homed in fx.mainDir
			state.AgentWorktree = tc.lastReported
			s.rehomeSessionToCurrentWorktree(signalled, repo, &state, false)
			if tc.moved {
				assert.Equal(t, fx.worktreeDir, state.WorktreePath)
			} else {
				assert.Equal(t, fx.mainDir, state.WorktreePath, "a repeated payload must not move the session")
			}
			assert.Equal(t, fx.worktreeDir, state.AgentWorktree, "the reported tree is recorded")
		})
	}
}
