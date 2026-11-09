package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// TestApplyTurnTokenUsage verifies the helper mirrors SaveStep's token
// accounting (accumulate into BOTH the session-cumulative TokenUsage and the
// checkpoint-scoped CheckpointTokenUsage). It exists for turns that end with
// no uncommitted changes (e.g. Antigravity committing all its work mid-turn):
// SaveStep is skipped, but the turn's token delta must still be recorded so
// the next condensation attributes it.
func TestApplyTurnTokenUsage(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		TokenUsage: &agent.TokenUsage{InputTokens: 100, OutputTokens: 10, APICallCount: 1},
	}

	ApplyTurnTokenUsage(state, &agent.TokenUsage{InputTokens: 50, OutputTokens: 5, APICallCount: 1}, nil)

	require.NotNil(t, state.TokenUsage)
	require.Equal(t, 150, state.TokenUsage.InputTokens, "session-cumulative input")
	require.Equal(t, 15, state.TokenUsage.OutputTokens, "session-cumulative output")
	require.Equal(t, 2, state.TokenUsage.APICallCount, "session-cumulative calls")
	require.NotNil(t, state.CheckpointTokenUsage, "checkpoint-scoped accumulator must be populated")
	require.Equal(t, 50, state.CheckpointTokenUsage.InputTokens, "checkpoint-scoped input")

	// Nil delta is a no-op.
	ApplyTurnTokenUsage(state, nil, nil)
	require.Equal(t, 150, state.TokenUsage.InputTokens)
	require.Equal(t, 50, state.CheckpointTokenUsage.InputTokens)
}

// TestApplyTurnTokenUsage_SubagentTokensMatchSaveStep pins that a
// checkpoint-less turn's cumulative subagent snapshot is handled as SaveStep
// handles it: it replaces the session's cumulative child total, the pending
// checkpoint window gets it rescoped against the baseline (so children already
// condensed are not counted again), and a snapshot computed against a stale
// subagent ledger version is dropped.
func TestApplyTurnTokenUsage_SubagentTokensMatchSaveStep(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		AgentType:              agent.AgentTypeClaudeCode,
		SubagentLedgerVersion:  2,
		TokenUsage:             &agent.TokenUsage{InputTokens: 100, SubagentTokens: &agent.TokenUsage{InputTokens: 30}},
		SubagentTokensBaseline: &agent.TokenUsage{InputTokens: 30},
	}

	ApplyTurnTokenUsage(state, &agent.TokenUsage{
		InputTokens: 10, APICallCount: 1, SubagentTokens: &agent.TokenUsage{InputTokens: 45},
	}, nil)

	require.Equal(t, 110, state.TokenUsage.InputTokens)
	require.NotNil(t, state.TokenUsage.SubagentTokens)
	require.Equal(t, 45, state.TokenUsage.SubagentTokens.InputTokens, "session child total is the latest cumulative snapshot")
	require.NotNil(t, state.CheckpointTokenUsage)
	require.Equal(t, 10, state.CheckpointTokenUsage.InputTokens)
	require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens)
	require.Equal(t, 15, state.CheckpointTokenUsage.SubagentTokens.InputTokens, "window child total is rescoped against the baseline (45-30)")

	stale := uint64(1)
	ApplyTurnTokenUsage(state, &agent.TokenUsage{
		InputTokens: 5, APICallCount: 1, SubagentTokens: &agent.TokenUsage{InputTokens: 999},
	}, &stale)

	require.Equal(t, 115, state.TokenUsage.InputTokens, "main-agent tokens survive a stale ledger version")
	if state.TokenUsage.SubagentTokens != nil {
		require.NotEqual(t, 999, state.TokenUsage.SubagentTokens.InputTokens, "a stale-ledger child snapshot must not be recorded")
	}
}
