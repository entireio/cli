package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// A turn end that fails after its claim must not strand the parent: the claim
// is released and the pause restored, so the next hook can try again. A turn
// that did reach IDLE keeps its claim, so it is never ended twice.
//
// Not parallel: uses t.Chdir()
func TestSubagentTurnEndClaim_ReleasedWhenTheTurnEndFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	paused := time.Now()
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:           "parent",
		BaseCommit:          "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		StartedAt:           paused,
		Phase:               session.PhaseActive,
		AwaitingSubagentsAt: &paused,
	}))
	load := func() *strategy.SessionState {
		t.Helper()
		state, err := strategy.LoadSessionState(ctx, "parent")
		require.NoError(t, err)
		return state
	}

	require.True(t, claimSubagentTurnEnd(ctx, "parent"))
	require.False(t, claimSubagentTurnEnd(ctx, "parent"), "claimed once")
	require.NoError(t, recordAwaitingSubagents(ctx, "parent", true))
	require.Nil(t, load().AwaitingSubagentsAt, "a pause cannot re-arm a claimed turn")

	// The turn end failed with the parent still ACTIVE: retryable again.
	require.NoError(t, releaseSubagentTurnEndClaim(ctx, "parent"))
	state := load()
	require.False(t, state.SubagentTurnEndClaimed)
	require.NotNil(t, state.AwaitingSubagentsAt)
	require.True(t, claimSubagentTurnEnd(ctx, "parent"), "the next hook can claim it")

	// The turn end reached IDLE before failing: it ended, so nothing is undone.
	require.NoError(t, strategy.MutateSessionState(ctx, "parent", func(state *strategy.SessionState) error {
		state.Phase = session.PhaseIdle
		return nil
	}))
	require.NoError(t, releaseSubagentTurnEndClaim(ctx, "parent"))
	require.True(t, load().SubagentTurnEndClaimed)
}

// A subagent past the stuck threshold may still be doing long work that fires
// no hook. With a live recorded owner it is not done, so its parent's turn
// keeps running; only an unknown owner falls back to the stuck heuristic, and
// an exited owner is done at once.
//
// Not parallel: uses t.Chdir()
func TestSubagentDone_StuckActiveChildNeedsADeadOrUnknownOwner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	stale := time.Now().Add(-session.StuckActiveThreshold - time.Minute)
	saveChild := func(owner *proclive.Identity) {
		t.Helper()
		require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
			SessionID:           "child",
			ParentSessionID:     "parent",
			BaseCommit:          "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			StartedAt:           stale,
			LastInteractionTime: &stale,
			Phase:               session.PhaseActive,
			Owner:               owner,
		}))
	}

	// No recorded owner: liveness is unknown, so the stuck heuristic decides.
	saveChild(nil)
	require.True(t, subagentDone(ctx, nil, "child", "parent", agent.SubagentExchange{}),
		"a stuck child with no owner counts as done")

	live, ok := proclive.IdentityOf(os.Getpid())
	if !ok {
		t.Skip("process liveness is unknown on this platform")
	}
	saveChild(&live)
	require.False(t, subagentDone(ctx, nil, "child", "parent", agent.SubagentExchange{}),
		"a stuck-looking child whose owner is alive is still working")

	dead := live
	dead.Start = "not-" + live.Start
	saveChild(&dead)
	require.True(t, subagentDone(ctx, nil, "child", "parent", agent.SubagentExchange{}),
		"a child whose owner exited is done")
}
