package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

	ApplyTurnTokenUsage(state, &agent.TokenUsage{InputTokens: 50, OutputTokens: 5, APICallCount: 1}, nil, nil)

	require.NotNil(t, state.TokenUsage)
	require.Equal(t, 150, state.TokenUsage.InputTokens, "session-cumulative input")
	require.Equal(t, 15, state.TokenUsage.OutputTokens, "session-cumulative output")
	require.Equal(t, 2, state.TokenUsage.APICallCount, "session-cumulative calls")
	require.NotNil(t, state.CheckpointTokenUsage, "checkpoint-scoped accumulator must be populated")
	require.Equal(t, 50, state.CheckpointTokenUsage.InputTokens, "checkpoint-scoped input")

	// Nil delta is a no-op.
	ApplyTurnTokenUsage(state, nil, nil, nil)
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
	}, nil, nil)

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
	}, &stale, nil)

	require.Equal(t, 115, state.TokenUsage.InputTokens, "main-agent tokens survive a stale ledger version")
	if state.TokenUsage.SubagentTokens != nil {
		require.NotEqual(t, 999, state.TokenUsage.SubagentTokens.InputTokens, "a stale-ledger child snapshot must not be recorded")
	}
}

// TestApplyTurnTokenUsage_ScopesPendingToTokenStart: a condensation during the
// turn (a mid-turn commit or attach) stored the turn so far and moved the token
// offset past the turn start. The session total still gets the whole turn, but
// the pending checkpoint window gets only the part recounted from the token
// offset, keeping the turn's cumulative subagent snapshot.
func TestApplyTurnTokenUsage_ScopesPendingToTokenStart(t *testing.T) {
	t.Parallel()
	complete := true
	turn := &agent.TokenUsage{
		InputTokens: 30, OutputTokens: 30, APICallCount: 3,
		SubagentTokens: &agent.TokenUsage{OutputTokens: 4}, SubagentTokensComplete: &complete,
	}
	six, eight := 6, 8

	for _, tc := range []struct {
		name        string
		tokenStart  int
		baseline    *int
		noRecount   bool
		recounted   int
		wantPending int
		wantFrom    int
	}{
		{name: "token offset moved during the turn", tokenStart: 8, recounted: 10, wantPending: 10, wantFrom: 8},
		{name: "token offset moved past its turn-start value", tokenStart: 8, baseline: &six, recounted: 10, wantPending: 10, wantFrom: 8},
		{name: "nothing after the token offset", tokenStart: 8, wantPending: 0, wantFrom: 8},
		{name: "token offset before the turn start", tokenStart: 3, recounted: 10, wantPending: 30},
		{name: "transcript shorter than an unmoved token offset", tokenStart: 8, baseline: &eight, recounted: 10, wantPending: 30},
		{name: "no recount (hook or out-of-band usage)", tokenStart: 8, noRecount: true, wantPending: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := &SessionState{}
			state.SetTokenStart(tc.tokenStart)
			from := 0
			var recount TurnTokenRecount
			if !tc.noRecount {
				recount = NewTurnTokenRecount(5, tc.baseline, func(f int) *agent.TokenUsage {
					from = f
					return &agent.TokenUsage{InputTokens: tc.recounted, OutputTokens: tc.recounted, APICallCount: 1}
				})
			}

			ApplyTurnTokenUsage(state, turn, nil, recount)

			require.NotNil(t, state.TokenUsage)
			require.Equal(t, 30, state.TokenUsage.OutputTokens, "session total gets the whole turn")
			require.NotNil(t, state.CheckpointTokenUsage, "a turn with usage always leaves a pending window")
			require.Equal(t, tc.wantPending, state.CheckpointTokenUsage.OutputTokens, "pending main-agent tokens")
			require.Equal(t, tc.wantPending, state.CheckpointTokenUsage.InputTokens, "pending main-agent tokens")
			require.Equal(t, tc.wantFrom, from, "recount offset")
			require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens, "the turn's subagent snapshot is kept")
			require.Equal(t, 4, state.CheckpointTokenUsage.SubagentTokens.OutputTokens)
		})
	}
}

// TestSaveStep_RescopesWithTokenStartAtApply: the token offset is read inside
// SaveStep's state mutation, so a condensation that moves it after the hook
// resolved the turn's usage still scopes the pending window.
func TestSaveStep_RescopesWithTokenStartAtApply(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "test.txt", "v1")
	testutil.GitAdd(t, dir, "test.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-10-20-pending-scope"
	metadataDir := ".entire/metadata/" + sessionID
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, metadataDir, paths.TranscriptFileName), []byte(`{"type":"user"}`+"\n"), 0o644))
	step := func(content string, usage *agent.TokenUsage, recount TurnTokenRecount) {
		t.Helper()
		testutil.WriteFile(t, dir, "test.txt", content)
		require.NoError(t, s.SaveStep(ctx, StepContext{
			SessionID: sessionID, MetadataDir: metadataDir, ModifiedFiles: []string{"test.txt"},
			CommitMessage: "step", AuthorName: "Test", AuthorEmail: "test@test.com",
			AgentType:  agent.AgentTypeClaudeCode,
			TokenUsage: usage, PendingTokenUsageFrom: recount,
		}))
	}
	step("v2", nil, nil)
	// The hook has resolved the turn's usage; a condensation now stores the
	// turn so far and moves the token offset before SaveStep takes the lock.
	require.NoError(t, MutateSessionState(ctx, sessionID, func(st *SessionState) error {
		st.CheckpointTokenUsage = nil
		st.SetTokenStart(8)
		return nil
	}))
	from := 0
	step("v3", &agent.TokenUsage{InputTokens: 30, OutputTokens: 30, APICallCount: 3}, NewTurnTokenRecount(5, nil, func(f int) *agent.TokenUsage {
		from = f
		return &agent.TokenUsage{InputTokens: 10, OutputTokens: 10, APICallCount: 1}
	}))

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 8, from, "recount from the token offset read under the lock")
	require.Equal(t, 30, state.TokenUsage.OutputTokens, "session total gets the whole turn")
	require.NotNil(t, state.CheckpointTokenUsage)
	require.Equal(t, 10, state.CheckpointTokenUsage.OutputTokens, "pending gets only what no checkpoint stored")
}
