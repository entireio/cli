package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/stretchr/testify/require"
)

const attachTestTranscript = `{"type":"assistant","uuid":"u1","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":10}}}` + "\n"

// attachTwoTurnTranscript's first line is what an earlier checkpoint counted;
// the second is new since TokenStart 1.
const attachTwoTurnTranscript = `{"type":"assistant","uuid":"u1","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":100}}}` + "\n" +
	`{"type":"assistant","uuid":"u2","message":{"id":"m2","usage":{"input_tokens":10,"output_tokens":10}}}` + "\n"

func TestAttachTokenUsage_KeepsPendingSubagentTokens(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:            "s",
		AgentType:            agent.AgentTypeClaudeCode,
		TokenUsage:           &agent.TokenUsage{OutputTokens: 10, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}},
		CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 10, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}},
	}
	usage, pos := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, state, []byte(attachTestTranscript), "")
	require.NotNil(t, usage.SubagentTokens, "the attach checkpoint must carry the pending subagent tokens")
	require.Equal(t, 50, usage.SubagentTokens.OutputTokens)

	ConsumeAttachTokenWindow(state, pos)
	require.Nil(t, state.CheckpointTokenUsage)
	require.Equal(t, 1, state.TokenStart())
}

func TestAttachTokenUsage_CountsFromTokenStart(t *testing.T) {
	t.Parallel()
	withState := &SessionState{SessionID: "s", AgentType: agent.AgentTypeClaudeCode}
	withState.SetTokenStart(1)
	usage, pos := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, withState, []byte(attachTwoTurnTranscript), "")
	require.Equal(t, 10, usage.OutputTokens, "tokens before TokenStart are in an earlier checkpoint")
	require.Equal(t, 2, pos)

	usage, _ = AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, nil, []byte(attachTwoTurnTranscript), "")
	require.Equal(t, 110, usage.OutputTokens, "without state the whole transcript is counted")

	// A state recreated from scratch (cleanup, resume) has counted nothing.
	fresh := &SessionState{SessionID: "s", AgentType: agent.AgentTypeClaudeCode}
	usage, _ = AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, fresh, []byte(attachTwoTurnTranscript), "")
	require.Equal(t, 110, usage.OutputTokens)
}
