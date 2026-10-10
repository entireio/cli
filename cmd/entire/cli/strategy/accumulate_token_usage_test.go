package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestAccumulateSessionTokenUsage verifies the helper mirrors SaveStep's token
// accounting (accumulate into BOTH the session-cumulative TokenUsage and the
// checkpoint-scoped CheckpointTokenUsage). It exists for turns that end with
// no uncommitted changes (e.g. Antigravity committing all its work mid-turn):
// SaveStep is skipped, but the turn's out-of-band token delta must still be
// recorded so the next condensation attributes it.
func TestAccumulateSessionTokenUsage(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	sessionID := "agy-accumulate-tokens"
	now := time.Now()
	state := &SessionState{
		SessionID:           sessionID,
		AgentType:           agenttypes.AgentType("Antigravity"),
		StartedAt:           now, // zero StartedAt would be auto-deleted as stale on load
		LastInteractionTime: &now,
		TokenUsage: &agent.TokenUsage{
			InputTokens: 100, OutputTokens: 10, APICallCount: 1,
		},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	delta := &agent.TokenUsage{InputTokens: 50, OutputTokens: 5, APICallCount: 1}
	require.NoError(t, AccumulateSessionTokenUsage(context.Background(), sessionID, delta, nil))

	got, err := LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got.TokenUsage)
	require.Equal(t, 150, got.TokenUsage.InputTokens, "session-cumulative input")
	require.Equal(t, 15, got.TokenUsage.OutputTokens, "session-cumulative output")
	require.Equal(t, 2, got.TokenUsage.APICallCount, "session-cumulative calls")
	require.NotNil(t, got.CheckpointTokenUsage, "checkpoint-scoped accumulator must be populated")
	require.Equal(t, 50, got.CheckpointTokenUsage.InputTokens, "checkpoint-scoped input")

	// Nil delta is a no-op, not an error.
	require.NoError(t, AccumulateSessionTokenUsage(context.Background(), sessionID, nil, nil))
}

// TestAccumulateSessionTokenUsage_SubagentTokensMatchSaveStep pins that a
// checkpoint-less turn's cumulative subagent snapshot is handled as SaveStep
// handles it: it replaces the session's cumulative child total, the pending
// checkpoint window gets it rescoped against the baseline (so children already
// condensed are not counted again), and a snapshot computed against a stale
// subagent ledger version is dropped.
func TestAccumulateSessionTokenUsage_SubagentTokensMatchSaveStep(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	ctx := context.Background()

	sessionID := "subagent-accumulate-tokens"
	now := time.Now()
	require.NoError(t, SaveSessionState(ctx, &SessionState{
		SessionID:              sessionID,
		AgentType:              agent.AgentTypeClaudeCode,
		StartedAt:              now,
		LastInteractionTime:    &now,
		SubagentLedgerVersion:  2,
		TokenUsage:             &agent.TokenUsage{InputTokens: 100, SubagentTokens: &agent.TokenUsage{InputTokens: 30}},
		SubagentTokensBaseline: &agent.TokenUsage{InputTokens: 30},
	}))

	require.NoError(t, AccumulateSessionTokenUsage(ctx, sessionID, &agent.TokenUsage{
		InputTokens: 10, APICallCount: 1, SubagentTokens: &agent.TokenUsage{InputTokens: 45},
	}, nil))

	got, err := LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 110, got.TokenUsage.InputTokens)
	require.NotNil(t, got.TokenUsage.SubagentTokens)
	require.Equal(t, 45, got.TokenUsage.SubagentTokens.InputTokens, "session child total is the latest cumulative snapshot")
	require.NotNil(t, got.CheckpointTokenUsage)
	require.Equal(t, 10, got.CheckpointTokenUsage.InputTokens)
	require.NotNil(t, got.CheckpointTokenUsage.SubagentTokens)
	require.Equal(t, 15, got.CheckpointTokenUsage.SubagentTokens.InputTokens, "window child total is rescoped against the baseline (45-30)")

	stale := uint64(1)
	require.NoError(t, AccumulateSessionTokenUsage(ctx, sessionID, &agent.TokenUsage{
		InputTokens: 5, APICallCount: 1, SubagentTokens: &agent.TokenUsage{InputTokens: 999},
	}, &stale))

	got, err = LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 115, got.TokenUsage.InputTokens, "main-agent tokens survive a stale ledger version")
	if got.TokenUsage.SubagentTokens != nil {
		require.NotEqual(t, 999, got.TokenUsage.SubagentTokens.InputTokens, "a stale-ledger child snapshot must not be recorded")
	}
}
