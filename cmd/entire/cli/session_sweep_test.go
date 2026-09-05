//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sweep predicate decides, from state files alone, whether a background
// sweep is worth spawning. False negatives mean zombies linger (the pre-sweep
// status quo); false positives spawn a process that no-ops — so the predicate
// leans conservative: fresh ENDED sessions are NOT zombies (PostCommit
// carry-forward must get its chance), and live sessions are never flagged.
func TestIsSweepableZombie(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-1 * time.Hour)

	// Dead-owner fixture: a LIVE pid with a mismatched start-time fingerprint
	// reads as PID reuse → LivenessDead. Do NOT use a negative/absent PID —
	// proclive.Check returns LivenessUnknown for PID <= 0 and OwnerExited()
	// then reports false. Same dead-owner fixture as session_finalize_test.go's
	// tests.
	deadOwner := &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}

	tests := []struct {
		name  string
		state session.State
		want  bool
	}{
		{
			name: "active with dead owner is a zombie",
			state: session.State{
				Phase: session.PhaseActive,
				Owner: deadOwner,
			},
			want: true,
		},
		{
			name: "active with no owner recorded is not a zombie (legacy state)",
			state: session.State{
				Phase: session.PhaseActive,
			},
			want: false,
		},
		{
			name: "ended uncondensed with steps, older than threshold, is a zombie",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
				EndedAt:   &old,
			},
			want: true,
		},
		{
			// entire import writes ENDED states with old EndedAt, steps, and
			// no BaseCommit; they are exempt from the stale purge, so without
			// this guard every session start would nominate them and spawn a
			// doomed sweep forever.
			name: "imported ended session is never a zombie (permanent historical record)",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
				EndedAt:   &old,
				Kind:      session.KindImported,
			},
			want: false,
		},
		{
			name: "ended uncondensed but fresh is NOT a zombie (carry-forward window)",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
				EndedAt:   &fresh,
			},
			want: false,
		},
		{
			name: "ended and fully condensed is not a zombie",
			state: session.State{
				Phase:          session.PhaseEnded,
				StepCount:      3,
				FullyCondensed: true,
				EndedAt:        &old,
			},
			want: false,
		},
		{
			name: "ended with zero steps is not a zombie (nothing to condense)",
			state: session.State{
				Phase:   session.PhaseEnded,
				EndedAt: &old,
			},
			want: false,
		},
		{
			name: "ended with task records but zero steps is a zombie",
			state: session.State{
				Phase:       session.PhaseEnded,
				EndedAt:     &old,
				TaskRecords: []session.TaskRecord{{ToolUseID: "task-1"}},
			},
			want: true,
		},
		{
			name: "ended with nil EndedAt falls back to LastInteractionTime",
			state: session.State{
				Phase:               session.PhaseEnded,
				StepCount:           3,
				LastInteractionTime: &old,
			},
			want: true,
		},
		{
			name: "ended with no timestamps at all is skipped (cannot age-gate)",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
			},
			want: false,
		},
		{
			name: "idle with dead owner is a zombie",
			state: session.State{
				Phase: session.PhaseIdle,
				Owner: deadOwner,
			},
			want: true,
		},
		{
			name: "imported non-ended session is never a zombie",
			state: session.State{
				Phase: session.PhaseIdle,
				Owner: deadOwner,
				Kind:  session.KindImported,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isSweepableZombie(&tt.state, now))
		})
	}
}

// TestRunSessionSweep_CondensesOldEndedZombie_LeavesFreshAlone — the
// regression this whole feature exists for: an ENDED session with uncondensed
// checkpoints and a shadow branch used to linger until a human ran
// `entire doctor --force` (a real one sat for 4 days). The sweep must fix the
// old one and must NOT touch a freshly-ended one, whose PostCommit
// carry-forward window is still open. Alongside those two it pins the sweep's
// other two load-bearing branches: the discard-protection gate (an old zombie
// WITHOUT a shadow branch is left exactly as-is — discards are doctor's, never
// the sweep's) and the finalize pass (an ACTIVE session whose owner process is
// gone is marked ENDED).
//
// Each fixture gets its own BaseCommit (and, where condensable, its own shadow
// ref) so condensing one fixture can't delete a shadow branch another fixture
// depends on — sharing one branch would make the "untouched" assertions pass
// coincidentally.
func TestRunSessionSweep_CondensesOldEndedZombie_LeavesFreshAlone(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	ctx := context.Background()

	const (
		freshBase    = "1111111111111111111111111111111111111111"
		noShadowBase = "2222222222222222222222222222222222222222"
		activeBase   = "3333333333333333333333333333333333333333"
	)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	createShadowBranchRef(t, repo, testBaseCommit, "")
	createShadowBranchRef(t, repo, freshBase, "")
	// The dead-owner fixture gets a shadow ref too: once finalized to ENDED it
	// would otherwise match the pre-existing orphan cleanup (ENDED, no shadow
	// branch, no LastCheckpointID) and vanish before we can assert its phase.
	createShadowBranchRef(t, repo, activeBase, "")

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-time.Hour)

	zombie := &strategy.SessionState{
		SessionID:  "2026-08-17-sweep-old-zombie",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  2,
		StartedAt:  old.Add(-time.Hour),
		EndedAt:    &old,
	}
	require.NoError(t, strategy.SaveSessionState(ctx, zombie))

	recent := &strategy.SessionState{
		SessionID:  "2026-08-17-sweep-fresh-ended",
		BaseCommit: freshBase,
		Phase:      session.PhaseEnded,
		StepCount:  2,
		StartedAt:  fresh.Add(-time.Hour),
		EndedAt:    &fresh,
	}
	require.NoError(t, strategy.SaveSessionState(ctx, recent))

	// Old ENDED zombie with NO shadow branch: uncondensed steps whose data is
	// already gone. Fixing it means discarding state — doctor's case, which
	// the sweep must never initiate. Pins the IsCondensableEndedSession gate
	// in runSessionSweep: without the gate, CondenseSessionByID's engine
	// clears the state when it finds no shadow branch. LastCheckpointID is set
	// so the pre-existing orphan cleanup in listAllSessionStates (which runs
	// as a side effect of condensing the other zombie) keeps the state and the
	// assertion isolates the SWEEP's behavior.
	noShadow := &strategy.SessionState{
		SessionID:        "2026-08-17-sweep-no-shadow",
		BaseCommit:       noShadowBase,
		Phase:            session.PhaseEnded,
		StepCount:        2,
		StartedAt:        old.Add(-time.Hour),
		EndedAt:          &old,
		LastCheckpointID: id.MustCheckpointID("abc123def456"),
	}
	require.NoError(t, strategy.SaveSessionState(ctx, noShadow))

	// ACTIVE session whose owner is dead (same dead-owner fixture as
	// session_finalize_test.go's tests: live PID + mismatched start
	// fingerprint reads as PID reuse → LivenessDead). Pins that
	// runSessionSweep actually runs the finalize pass.
	activeDead := &strategy.SessionState{
		SessionID:  "2026-08-17-sweep-active-dead-owner",
		BaseCommit: activeBase,
		Phase:      session.PhaseActive,
		StartedAt:  old,
		Owner:      &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"},
	}
	require.NoError(t, strategy.SaveSessionState(ctx, activeDead))

	require.NoError(t, runSessionSweep(ctx))

	states, err := strategy.ListSessionStates(ctx)
	require.NoError(t, err)

	byID := map[string]*strategy.SessionState{}
	for _, st := range states {
		byID[st.SessionID] = st
	}

	// The fresh session must be exactly as we left it.
	freshAfter, ok := byID["2026-08-17-sweep-fresh-ended"]
	require.True(t, ok, "fresh ended session must survive the sweep untouched")
	assert.Equal(t, session.PhaseEnded, freshAfter.Phase)
	assert.False(t, freshAfter.FullyCondensed)
	assert.Equal(t, 2, freshAfter.StepCount)

	// The no-shadow zombie must be left exactly as-is: still ENDED, steps
	// intact, not marked condensed. The sweep never initiates a discard.
	noShadowAfter, ok := byID["2026-08-17-sweep-no-shadow"]
	require.True(t, ok, "no-shadow ended session must survive the sweep untouched")
	assert.Equal(t, session.PhaseEnded, noShadowAfter.Phase)
	assert.Equal(t, 2, noShadowAfter.StepCount)
	assert.False(t, noShadowAfter.FullyCondensed)

	// The dead-owner ACTIVE session must have been finalized to ENDED.
	activeAfter, ok := byID["2026-08-17-sweep-active-dead-owner"]
	require.True(t, ok, "dead-owner active session must still exist after the sweep")
	assert.Equal(t, session.PhaseEnded, activeAfter.Phase,
		"sweep must finalize an ACTIVE session whose owner process is gone")

	// The old zombie must no longer be flagged as a condensable zombie...
	if zombieAfter, exists := byID["2026-08-17-sweep-old-zombie"]; exists {
		assert.False(t, strategy.IsCondensableEndedSession(repo, zombieAfter),
			"old zombie must not remain condensable after the sweep")
		// ...and its state must show CondenseSessionByID actually ran, not
		// that the predicate merely stopped matching: the skip path (no
		// transcript/files — this fixture) sets FullyCondensed=true and keeps
		// PhaseEnded; the full condense path resets Phase to IDLE (and
		// StepCount to 0). Either way the ENDED+uncondensed combination is
		// gone.
		assert.True(t, zombieAfter.FullyCondensed || zombieAfter.Phase != session.PhaseEnded,
			"swept zombie must be marked fully condensed or moved out of ENDED, got phase=%s fullyCondensed=%v",
			zombieAfter.Phase, zombieAfter.FullyCondensed)
	}
	// (absence from the list is also success: fully cleaned up)
}

// countSweepableZombies is the hook-side spawn decision. It must be a pure
// function over the state list: SpawnDetached is untestable in-process (it
// no-ops under `go test`), so correctness of "would we spawn?" lives here.
func TestSessionSweepNeeded(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-48 * time.Hour)

	assert.Equal(t, 0, countSweepableZombies(nil, now), "no sessions → no sweep")

	healthy := &session.State{Phase: session.PhaseIdle}
	assert.Equal(t, 0, countSweepableZombies([]*session.State{healthy}, now))

	zombie := &session.State{Phase: session.PhaseEnded, StepCount: 1, EndedAt: &old}
	assert.Equal(t, 1, countSweepableZombies([]*session.State{healthy, zombie}, now),
		"one zombie among healthy sessions → sweep")
}

// TestMaybeSpawnSessionSweep_SeamAndThrottle pins the spawn decision end to
// end through the sweepSpawn seam: no zombies → no spawn; a zombie → exactly
// one spawn, handed the worktree root; a fresh throttle marker → no second
// spawn (a burst of session starts, or a persistently failing condense, must
// collapse to one child per throttle window).
func TestMaybeSpawnSessionSweep_SeamAndThrottle(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the package-level seam swap are
	// process-global.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	ctx := context.Background()

	var spawns atomic.Int32
	var gotRoot atomic.Value
	prevSpawn := sweepSpawn
	prevMetadata := sweepWorktreeMetadata
	sweepSpawn = func(worktreeRoot string) {
		spawns.Add(1)
		gotRoot.Store(worktreeRoot)
	}
	t.Cleanup(func() {
		sweepSpawn = prevSpawn
		sweepWorktreeMetadata = prevMetadata
	})

	// No zombies: no spawn (and no throttle marker written — the throttle is
	// only consulted once a zombie nominates).
	maybeSpawnSessionSweep(ctx)
	assert.Equal(t, int32(0), spawns.Load(), "no zombies must not spawn a sweep")

	old := time.Now().Add(-48 * time.Hour)
	zombie := &strategy.SessionState{
		SessionID:  "2026-08-17-spawn-seam-zombie",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  1,
		StartedAt:  old.Add(-time.Hour),
		EndedAt:    &old,
	}
	require.NoError(t, strategy.SaveSessionState(ctx, zombie))

	// A common-dir failure means there is no safe repository-wide throttle key.
	// Fail closed rather than forking an unbounded child on every session start.
	wantRoot, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	sweepWorktreeMetadata = func(root string) (gitrepo.WorktreeMetadata, error) {
		require.Equal(t, wantRoot, root, "metadata resolution must use the discovered worktree root")
		return gitrepo.WorktreeMetadata{}, errors.New("common dir unavailable")
	}
	maybeSpawnSessionSweep(ctx)
	assert.Equal(t, int32(0), spawns.Load(), "common-dir failure must not spawn a sweep")
	sweepWorktreeMetadata = prevMetadata

	commonFile := filepath.Join(dir, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))
	maybeSpawnSessionSweep(ctx)
	assert.Equal(t, int32(0), spawns.Load(), "broken metadata must not spawn a sweep")
	require.NoError(t, os.Remove(commonFile))

	// Zombie present: exactly one spawn, with the worktree root.
	maybeSpawnSessionSweep(ctx)
	require.Equal(t, int32(1), spawns.Load(), "a zombie must spawn exactly one sweep")
	got, okRoot := gotRoot.Load().(string)
	require.True(t, okRoot, "seam must have been handed a worktree root")
	require.NotEmpty(t, got)
	gotEval, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, wantRoot, gotEval, "sweep must be spawned from the worktree root")

	// Throttle marker is now fresh: the zombie still nominates, but no second
	// child is forked within the window.
	maybeSpawnSessionSweep(ctx)
	assert.Equal(t, int32(1), spawns.Load(),
		"a second session start within the throttle window must not spawn another sweep")
}

// TestSweepSessionsCommand_Registered pins that the literal command name
// spawned by spawnDetachedSessionSweepProcess matches a command registered on
// the root: a typo'd name would make every spawn a permanent silent production
// no-op (the detached child exits with "unknown command" into a discarded
// stderr, forever). SpawnDetached itself no-ops under `go test`, so this
// drives the root command directly, which runs the sweep inline — fine: empty
// repo, no zombies.
func TestSweepSessionsCommand_Registered(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	root := NewRootCmd()
	root.SetArgs([]string{"__sweep_sessions"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	require.NoError(t, root.Execute())
}
