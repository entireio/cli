package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/stretchr/testify/require"
)

const attachTestTranscript = `{"type":"assistant","uuid":"u1","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":10}}}` + "\n"

func TestAttachTokenUsage_KeepsPendingSubagentTokens(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:            "s",
		AgentType:            agent.AgentTypeClaudeCode,
		TokenUsage:           &agent.TokenUsage{OutputTokens: 10, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}},
		CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 10, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}},
	}
	usage, pos := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, state, []byte(attachTestTranscript), nil)
	require.NotNil(t, usage.SubagentTokens, "the attach checkpoint must carry the pending subagent tokens")
	require.Equal(t, 50, usage.SubagentTokens.OutputTokens)

	ConsumeAttachTokenWindow(state, pos)
	require.Nil(t, state.CheckpointTokenUsage)
	require.Equal(t, 1, state.TokenStart())
}

func TestAttachTokenUsage_ReplacedEntry(t *testing.T) {
	t.Parallel()
	replaced := &agent.TokenUsage{InputTokens: 100, OutputTokens: 100}

	withState := &SessionState{SessionID: "s", AgentType: agent.AgentTypeClaudeCode}
	usage, _ := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, withState, []byte(attachTestTranscript), replaced)
	require.Equal(t, 110, usage.OutputTokens, "with state, the replaced entry's tokens precede TokenStart and are kept")

	usage, _ = AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, nil, []byte(attachTestTranscript), replaced)
	require.Equal(t, 10, usage.OutputTokens, "without state the whole transcript is counted, which already covers the entry")
}
