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
	require.NoError(t, AccumulateSessionTokenUsage(context.Background(), sessionID, delta))

	got, err := LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got.TokenUsage)
	require.Equal(t, 150, got.TokenUsage.InputTokens, "session-cumulative input")
	require.Equal(t, 15, got.TokenUsage.OutputTokens, "session-cumulative output")
	require.Equal(t, 2, got.TokenUsage.APICallCount, "session-cumulative calls")
	require.NotNil(t, got.CheckpointTokenUsage, "checkpoint-scoped accumulator must be populated")
	require.Equal(t, 50, got.CheckpointTokenUsage.InputTokens, "checkpoint-scoped input")

	// Nil delta is a no-op, not an error.
	require.NoError(t, AccumulateSessionTokenUsage(context.Background(), sessionID, nil))
}
