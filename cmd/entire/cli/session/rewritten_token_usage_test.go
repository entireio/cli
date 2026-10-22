package session

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

func TestState_CarryRewrittenTokenUsage_MovesMainAgentCounters(t *testing.T) {
	t.Parallel()
	complete := true
	s := &State{CheckpointTokenUsage: &agent.TokenUsage{
		InputTokens: 2, CacheCreationTokens: 745, CacheReadTokens: 37157, OutputTokens: 44, APICallCount: 1,
		SubagentTokens: &agent.TokenUsage{OutputTokens: 50}, SubagentTokensComplete: &complete,
	}}

	s.CarryRewrittenTokenUsage()

	require.Equal(t, &agent.TokenUsage{
		InputTokens: 2, CacheCreationTokens: 745, CacheReadTokens: 37157, OutputTokens: 44, APICallCount: 1,
	}, s.RewrittenTokenUsage)
	require.Equal(t, &agent.TokenUsage{
		SubagentTokens: &agent.TokenUsage{OutputTokens: 50}, SubagentTokensComplete: &complete,
	}, s.CheckpointTokenUsage, "the subagent total stays pending")
}

func TestState_CarryRewrittenTokenUsage_Accumulates(t *testing.T) {
	t.Parallel()
	s := &State{CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 10, APICallCount: 1}}
	s.CarryRewrittenTokenUsage()
	require.Nil(t, s.CheckpointTokenUsage, "nothing stays pending without a subagent total")

	s.CheckpointTokenUsage = &agent.TokenUsage{OutputTokens: 5, APICallCount: 1}
	s.CarryRewrittenTokenUsage()

	require.Equal(t, &agent.TokenUsage{OutputTokens: 15, APICallCount: 2}, s.RewrittenTokenUsage)
}

func TestState_CarryRewrittenTokenUsage_NoMainAgentUsage(t *testing.T) {
	t.Parallel()
	s := &State{}
	s.CarryRewrittenTokenUsage()
	require.Nil(t, s.RewrittenTokenUsage)

	s.CheckpointTokenUsage = &agent.TokenUsage{SubagentTokens: &agent.TokenUsage{OutputTokens: 50}}
	s.CarryRewrittenTokenUsage()
	require.Nil(t, s.RewrittenTokenUsage, "a subagent-only window carries nothing")
	require.NotNil(t, s.CheckpointTokenUsage)
}

func TestState_RewrittenTokenUsage_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(&State{SessionID: "s"})
	require.NoError(t, err)
	require.NotContains(t, string(data), "rewritten_token_usage", "omitted when empty")

	data, err = json.Marshal(&State{SessionID: "s", RewrittenTokenUsage: &agent.TokenUsage{OutputTokens: 44}})
	require.NoError(t, err)
	var got State
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, &agent.TokenUsage{OutputTokens: 44}, got.RewrittenTokenUsage)
}
