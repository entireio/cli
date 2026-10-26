package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/stretchr/testify/require"
)

// rewrittenUsageState returns a state holding pending usage a transcript
// rewrite removed, plus a pending window with a subagent total.
func rewrittenUsageState() *SessionState {
	return &SessionState{
		SessionID:            "s",
		AgentType:            agent.AgentTypeClaudeCode,
		TokenUsage:           &agent.TokenUsage{OutputTokens: 30, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}},
		CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 10, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}},
		RewrittenTokenUsage:  &agent.TokenUsage{InputTokens: 2, OutputTokens: 20, APICallCount: 1},
	}
}

func TestResolveCondensedTokenUsage_AddsRewrittenUsage(t *testing.T) {
	t.Parallel()
	for name, recount := range map[string]*agent.TokenUsage{
		"recount finds usage": {InputTokens: 1, OutputTokens: 10, APICallCount: 1},
		"recount falls back":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := rewrittenUsageState()
			sessionData := &ExtractedSessionData{TokenUsage: recount}

			resolveCondensedTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, state, sessionData)

			got := sessionData.TokenUsage
			require.NotNil(t, got)
			require.Equal(t, 30, got.OutputTokens, "the checkpoint holds this window's usage plus the rewritten-away usage")
			require.NotNil(t, got.SubagentTokens)
			require.Equal(t, 50, got.SubagentTokens.OutputTokens, "the pending subagent total is kept")
			require.Equal(t, 20, state.RewrittenTokenUsage.OutputTokens,
				"the state keeps it until the window reset, so a failed write retries with it")

			resetCheckpointWindow(state)
			require.Nil(t, state.RewrittenTokenUsage, "the reset clears it once a checkpoint stored it")
		})
	}
}

func TestAttachTokenUsage_AddsRewrittenUsage(t *testing.T) {
	t.Parallel()
	state := rewrittenUsageState()

	usage, pos := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, state, []byte(attachTestTranscript), "")
	require.Equal(t, 30, usage.OutputTokens, "the attach checkpoint holds the rewritten-away usage")
	require.Equal(t, 20, state.RewrittenTokenUsage.OutputTokens)

	ConsumeAttachTokenWindow(state, pos)
	require.Nil(t, state.RewrittenTokenUsage)
}
