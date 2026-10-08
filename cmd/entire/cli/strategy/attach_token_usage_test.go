package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/stretchr/testify/require"
)

const attachTestTranscript = `{"type":"assistant","uuid":"u1","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":10}}}` + "\n"

// attachTwoTurnTranscript's first line is what an earlier checkpoint counted
// (the replaced entry's tokens); the second is new since TokenStart 1.
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
	withState.SetTokenStart(1)
	usage, _ := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, withState, []byte(attachTwoTurnTranscript), replaced)
	require.Equal(t, 110, usage.OutputTokens, "with state, the replaced entry's tokens precede TokenStart and are kept")

	usage, _ = AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, nil, []byte(attachTwoTurnTranscript), replaced)
	require.Equal(t, 110, usage.OutputTokens, "without state the whole transcript is counted, which already covers the entry")

	// A state recreated from scratch (cleanup, resume) has counted nothing, so
	// the whole transcript is counted, as without state, not added to the entry.
	fresh := &SessionState{SessionID: "s", AgentType: agent.AgentTypeClaudeCode}
	usage, _ = AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, fresh, []byte(attachTwoTurnTranscript), replaced)
	require.Equal(t, 110, usage.OutputTokens, "a fresh state must not count the replaced entry twice")
}

func TestAttachTokenUsage_ReplacedEntryKeepsSubagentTokens(t *testing.T) {
	t.Parallel()
	// Both subagent totals are window deltas: the replaced entry's is what that
	// checkpoint stored, the pending one is what the session added since.
	replaced := &agent.TokenUsage{OutputTokens: 100, SubagentTokens: &agent.TokenUsage{OutputTokens: 50}}
	state := &SessionState{
		SessionID:            "s",
		AgentType:            agent.AgentTypeClaudeCode,
		CheckpointTokenUsage: &agent.TokenUsage{SubagentTokens: &agent.TokenUsage{OutputTokens: 5}},
	}
	state.SetTokenStart(1)
	usage, _ := AttachTokenUsage(context.Background(), &claudecode.ClaudeCodeAgent{}, state, []byte(attachTwoTurnTranscript), replaced)
	require.Equal(t, 110, usage.OutputTokens)
	require.NotNil(t, usage.SubagentTokens)
	require.Equal(t, 55, usage.SubagentTokens.OutputTokens, "the replaced entry's subagent tokens must be added, not overwritten")
}
