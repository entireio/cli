package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// writeLedgerTranscript writes a Claude Code transcript whose messages used
// the given output tokens (input equal to output).
func writeLedgerTranscript(t *testing.T, path string, outputs ...int) {
	t.Helper()
	var data []byte
	for i, out := range outputs {
		data = append(data, []byte(`{"type":"assistant","uuid":"u`+strconv.Itoa(i)+`","message":{"id":"m`+strconv.Itoa(i)+`","usage":{"input_tokens":`+strconv.Itoa(out)+`,"output_tokens":`+strconv.Itoa(out)+`}}}`+"\n")...)
	}
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func ledgerState(t *testing.T) *SessionState {
	t.Helper()
	return &SessionState{
		SessionID:      "ledger-session",
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: filepath.Join(t.TempDir(), "ledger-session.jsonl"),
	}
}

func TestNewTokenLedger_HistoryBeforeTheSessionCountsAsCheckpointed(t *testing.T) {
	t.Parallel()
	state := ledgerState(t)

	fresh := newTokenLedger(context.Background(), state.AgentType, state.SessionID, state.TranscriptPath)
	require.NotNil(t, fresh, "an agent with a running total gets a ledger")
	require.False(t, fresh.IsActive(), "nothing read yet: the per-turn path stays until usage appears")

	writeLedgerTranscript(t, state.TranscriptPath, 100)
	resumed := newTokenLedger(context.Background(), state.AgentType, state.SessionID, state.TranscriptPath)
	require.True(t, resumed.IsActive())
	require.Nil(t, resumed.Pending(), "a resumed conversation's earlier calls were counted by an earlier session state")
}

func TestObserveTokenLedger_TakesOverFromThePerTurnAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		legacyTotal   int
		legacyPending int
	}{
		{name: "everything checkpointed", legacyTotal: 100},
		{name: "a turn still pending", legacyTotal: 100, legacyPending: 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := ledgerState(t)
			state.TokenLedger = &session.TokenLedger{}
			state.TokenUsage = &agent.TokenUsage{InputTokens: tc.legacyTotal, OutputTokens: tc.legacyTotal}
			if tc.legacyPending > 0 {
				state.CheckpointTokenUsage = &agent.TokenUsage{InputTokens: tc.legacyPending, OutputTokens: tc.legacyPending}
			}
			// The per-turn path counted 100; the transcript also holds a call
			// it never saw (the agent began writing usage).
			writeLedgerTranscript(t, state.TranscriptPath, 60, 40, 10)

			require.True(t, ObserveTokenLedger(context.Background(), state))
			require.Nil(t, state.CheckpointTokenUsage, "the per-turn pending window hands over to the ledger")
			require.Equal(t, 110, state.TokenUsage.OutputTokens, "the session total is the agent's own")
			pending := state.TokenLedger.Pending()
			require.NotNil(t, pending)
			require.Equal(t, 110-(tc.legacyTotal-tc.legacyPending), pending.OutputTokens,
				"only what the per-turn path did not checkpoint is pending")
		})
	}
}

func TestObserveTokenLedger_StaysOffWithoutUsage(t *testing.T) {
	t.Parallel()
	state := ledgerState(t)
	state.TokenLedger = &session.TokenLedger{}
	state.TokenUsage = &agent.TokenUsage{OutputTokens: 7}
	require.NoError(t, os.WriteFile(state.TranscriptPath, []byte(`{"type":"user","uuid":"u","message":{"content":"hi"}}`+"\n"), 0o600))

	require.False(t, ObserveTokenLedger(context.Background(), state))
	require.Equal(t, 7, state.TokenUsage.OutputTokens, "the per-turn total is left alone")
}

func TestAttachTokenUsage_LedgerSessionStoresItsPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := ledgerState(t)
	writeLedgerTranscript(t, state.TranscriptPath, 100)
	state.TokenLedger = newTokenLedger(ctx, state.AgentType, state.SessionID, state.TranscriptPath)
	writeLedgerTranscript(t, state.TranscriptPath, 100, 20)

	transcript, err := os.ReadFile(state.TranscriptPath)
	require.NoError(t, err)
	usage, pos := AttachTokenUsage(ctx, &claudecode.ClaudeCodeAgent{}, state, transcript, state.TranscriptPath)
	require.Equal(t, 20, usage.OutputTokens, "attach stores what no checkpoint holds")
	require.Equal(t, 2, pos)

	read := state.TokenLedger.Clone()
	ConsumeAttachTokenWindow(state, pos)
	require.True(t, state.TokenLedger.HasPending(), "the caller marks the ledger, up to what attach read")
	state.TokenLedger.MarkCheckpointedUpTo(read)
	require.False(t, state.TokenLedger.HasPending())
}
