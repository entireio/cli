package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// subagentCostAgent is a minimal stand-in for a subagent-spawning agent (Claude
// Code, Factory AI Droid): its flat total carries a cumulative-since-session-
// start SubagentTokens snapshot (the SubagentAwareExtractor contract) while its
// per-model buckets are per-window deltas covering only the main transcript.
// Both fields are driven directly by the test so the turn-over-turn snapshot
// behaviour is exact rather than reconstructed from transcript fixtures.
type subagentCostAgent struct {
	flat    *agent.TokenUsage
	buckets []agent.ModelUsage
}

var (
	_ agent.Agent                  = (*subagentCostAgent)(nil)
	_ agent.SubagentAwareExtractor = (*subagentCostAgent)(nil)
	_ agent.ModelUsageCalculator   = (*subagentCostAgent)(nil)
)

func (a *subagentCostAgent) Name() types.AgentName                          { return "subagent-cost-test" }
func (a *subagentCostAgent) Type() types.AgentType                          { return agent.AgentTypeClaudeCode }
func (a *subagentCostAgent) Description() string                            { return "subagent cost accumulation test agent" }
func (a *subagentCostAgent) IsPreview() bool                                { return true }
func (a *subagentCostAgent) DetectPresence(_ context.Context) (bool, error) { return true, nil }
func (a *subagentCostAgent) ProtectedDirs() []string                        { return nil }
func (a *subagentCostAgent) ReadTranscript(string) ([]byte, error)          { return nil, nil }
func (a *subagentCostAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}

func (a *subagentCostAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out, nil
}

func (a *subagentCostAgent) GetSessionID(*agent.HookInput) string { return "" }
func (a *subagentCostAgent) GetSessionDir(string) (string, error) { return "", nil }
func (a *subagentCostAgent) ResolveSessionFile(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, sessionID+".jsonl")
}

func (a *subagentCostAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // not used by this test agent
}
func (a *subagentCostAgent) WriteSession(context.Context, *agent.AgentSession) error { return nil }
func (a *subagentCostAgent) FormatResumeCommand(string) string                       { return "" }

func (a *subagentCostAgent) ExtractAllModifiedFiles([]byte, int, string) ([]string, error) {
	return nil, nil
}

func (a *subagentCostAgent) CalculateTotalTokenUsage([]byte, int, string) (*agent.TokenUsage, error) {
	return a.flat, nil
}

func (a *subagentCostAgent) CalculateModelUsage([]byte, int) ([]agent.ModelUsage, error) {
	return a.buckets, nil
}

// TestSaveStep_SubagentRemainderNotInflatedAcrossTurns is the end-to-end
// regression test for the unbounded additive inflation of subagent cost and
// per-model tokens across a multi-turn session. It drives the real production
// loop for four turns: read the session's already-accounted subagent snapshot
// (as handleTurnEnd does), call agent.CalculateUsageWithCost, hand the result to
// SaveStep, and let accumulateTokenUsage / accumulateModelUsage fold it in.
//
// The mechanism under test: CalculateUsageWithCost's remainder bucket exists to
// price the subagent tokens that a ModelUsageCalculator's main-transcript-only
// buckets never see. flat.SubagentTokens is a cumulative-since-session-start
// snapshot, so before the fix the remainder re-attributed the ENTIRE subagent
// total on every turn, and accumulateModelUsage / accumulateTokenUsage — which
// correctly ADD genuine per-turn buckets — stacked those repeats. Cost,
// per-model tokens, state.TokenUsage and state.CheckpointTokenUsage all grew
// without bound even though the subagent did nothing after turn 1.
func TestSaveStep_SubagentRemainderNotInflatedAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v0"), 0o644))
	_, err = worktree.Add("test.txt")
	require.NoError(t, err)
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-08-10-subagent-remainder-delta"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(`{"type":"human","message":{"content":"test"}}`+"\n"), 0o644))

	// $1/MTok in and out, so token counts read directly as dollars per MTok.
	table, err := pricing.LoadTable([]pricing.ModelRate{
		{ID: "test-a", Provider: "test", InputPerMTok: 1, OutputPerMTok: 1},
	})
	require.NoError(t, err)

	// The main agent spends 1M input tokens per turn. The subagent's cumulative
	// total is 5M for the first three turns (it did no further work, but its
	// transcript is still re-read from line 0 every turn) and grows to 7M on the
	// fourth.
	subagentCumulative := []int{5_000_000, 5_000_000, 5_000_000, 7_000_000}

	for turn, cumulative := range subagentCumulative {
		// Exactly what handleTurnEnd does: the previously-accounted cumulative
		// subagent snapshot comes from the session-wide total, which
		// accumulateTokenUsage keeps at the latest snapshot.
		var accounted *agent.TokenUsage
		if state, stateErr := LoadSessionState(ctx, sessionID); stateErr == nil && state != nil && state.TokenUsage != nil {
			accounted = state.TokenUsage.SubagentTokens
		}

		ag := &subagentCostAgent{
			flat: &agent.TokenUsage{
				InputTokens:    1_000_000,
				APICallCount:   1,
				SubagentTokens: &agent.TokenUsage{InputTokens: cumulative, APICallCount: 5},
			},
			buckets: []agent.ModelUsage{
				{Model: "test-a", TokenUsage: agent.TokenUsage{InputTokens: 1_000_000, APICallCount: 1}},
			},
		}

		usage, buckets, costErr := agent.CalculateUsageWithCost(
			ag, nil, 0, "subagents", accounted, table, "test-a", false)
		require.NoError(t, costErr)

		// A fresh file body per turn so SaveStep sees a real diff rather than
		// skipping the checkpoint.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte{'v', byte('1' + turn)}, 0o644))
		require.NoError(t, s.SaveStep(ctx, StepContext{
			SessionID:      sessionID,
			MetadataDir:    metadataDir,
			MetadataDirAbs: metadataDirAbs,
			ModifiedFiles:  []string{"test.txt"},
			CommitMessage:  "turn checkpoint",
			AuthorName:     "Test",
			AuthorEmail:    "test@test.com",
			AgentType:      agent.AgentTypeClaudeCode,
			TokenUsage:     usage,
			ModelUsage:     buckets,
		}))
	}

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)

	// Main-agent tokens are genuine per-turn deltas and still sum: 4 x 1M.
	require.NotNil(t, state.TokenUsage)
	require.Equal(t, 4_000_000, state.TokenUsage.InputTokens,
		"main-agent input tokens must still sum across turns")

	// The session-wide subagent total is the latest cumulative snapshot, not a sum.
	require.NotNil(t, state.TokenUsage.SubagentTokens)
	require.Equal(t, 7_000_000, state.TokenUsage.SubagentTokens.InputTokens,
		"session-wide subagent total must be the latest cumulative snapshot")

	// Correct cost: 4M main + 7M subagent = 11M input at $1/MTok = $11.00.
	// Pre-fix the remainder re-attributed the whole snapshot every turn
	// (5+5+5+7 = 22M subagent), giving $26.00 — and it scales with turn count,
	// so a longer session inflates further without bound.
	require.NotNil(t, state.TokenUsage.CostUSD)
	require.InDelta(t, 11.0, *state.TokenUsage.CostUSD, 1e-9,
		"session-wide cost must count each subagent token once; 26.0 is the pre-fix inflated value")

	// CheckpointTokenUsage (the pending-checkpoint delta) accumulates the same
	// per-turn stream with no condensation in between, so it must match.
	require.NotNil(t, state.CheckpointTokenUsage)
	require.NotNil(t, state.CheckpointTokenUsage.CostUSD)
	require.InDelta(t, 11.0, *state.CheckpointTokenUsage.CostUSD, 1e-9,
		"checkpoint-scoped cost must count each subagent token once")
	require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens)
	require.Equal(t, 7_000_000, state.CheckpointTokenUsage.SubagentTokens.InputTokens,
		"no condensation happened, so the checkpoint window's subagent delta is the full 7M")

	// The per-model mirror is the aggregate that actually persists into
	// checkpoint metadata, and it inflated in tokens as well as cost.
	require.Len(t, state.ModelUsage, 1)
	perModel := state.ModelUsage["test-a"]
	require.NotNil(t, perModel)
	require.Equal(t, 11_000_000, perModel.InputTokens,
		"per-model tokens must total 4M main + 7M subagent; 22_000_000+ is the pre-fix inflated value")
	require.NotNil(t, perModel.CostUSD)
	require.InDelta(t, 11.0, *perModel.CostUSD, 1e-9, "per-model cost must match the token total")
}

// TestSaveStep_SubagentRemainderRescopedAcrossCondensation proves the single
// session-wide baseline is also correct for the checkpoint-scoped aggregate
// across a condensation reset. resetCheckpointWindow zeroes
// CheckpointTokenUsage/ModelUsage and rebaselines SubagentTokens, so the new
// window's totals come from the per-turn increments that land after the reset —
// they must not re-include subagent tokens the previous window already priced.
//
// This is why the remainder must NOT baseline on
// SessionState.SubagentTokensBaseline: that would re-attribute the whole window's
// subagent total on every turn inside the window, reproducing the same bug one
// scope smaller.
func TestSaveStep_SubagentRemainderRescopedAcrossCondensation(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v0"), 0o644))
	_, err = worktree.Add("test.txt")
	require.NoError(t, err)
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-08-10-subagent-remainder-condensation"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(`{"type":"human","message":{"content":"test"}}`+"\n"), 0o644))

	table, err := pricing.LoadTable([]pricing.ModelRate{
		{ID: "test-a", Provider: "test", InputPerMTok: 1, OutputPerMTok: 1},
	})
	require.NoError(t, err)

	saveTurn := func(t *testing.T, body string, cumulative int) {
		t.Helper()
		var accounted *agent.TokenUsage
		if state, stateErr := LoadSessionState(ctx, sessionID); stateErr == nil && state != nil && state.TokenUsage != nil {
			accounted = state.TokenUsage.SubagentTokens
		}
		ag := &subagentCostAgent{
			flat: &agent.TokenUsage{
				InputTokens:    1_000_000,
				APICallCount:   1,
				SubagentTokens: &agent.TokenUsage{InputTokens: cumulative, APICallCount: 5},
			},
			buckets: []agent.ModelUsage{
				{Model: "test-a", TokenUsage: agent.TokenUsage{InputTokens: 1_000_000, APICallCount: 1}},
			},
		}
		usage, buckets, costErr := agent.CalculateUsageWithCost(
			ag, nil, 0, "subagents", accounted, table, "test-a", false)
		require.NoError(t, costErr)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte(body), 0o644))
		require.NoError(t, s.SaveStep(ctx, StepContext{
			SessionID:      sessionID,
			MetadataDir:    metadataDir,
			MetadataDirAbs: metadataDirAbs,
			ModifiedFiles:  []string{"test.txt"},
			CommitMessage:  "turn checkpoint",
			AuthorName:     "Test",
			AuthorEmail:    "test@test.com",
			AgentType:      agent.AgentTypeClaudeCode,
			TokenUsage:     usage,
			ModelUsage:     buckets,
		}))
	}

	// Window 1: the subagent is discovered at 5M and does nothing more.
	saveTurn(t, "v1", 5_000_000)
	saveTurn(t, "v2", 5_000_000)

	// Condensation: window totals are cleared and the cumulative subagent total
	// is snapshotted as the new window's baseline.
	require.NoError(t, MutateSessionState(ctx, sessionID, func(st *SessionState) error {
		resetCheckpointWindow(st)
		return nil
	}))
	stateAfterReset, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, stateAfterReset.SubagentTokensBaseline)
	require.Equal(t, 5_000_000, stateAfterReset.SubagentTokensBaseline.InputTokens)

	// Window 2: one turn with no new subagent work, then one where it grows to 8M.
	saveTurn(t, "v3", 5_000_000)
	saveTurn(t, "v4", 8_000_000)

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)

	// Session-wide: 4M main + 8M subagent = $12.00.
	require.NotNil(t, state.TokenUsage.CostUSD)
	require.InDelta(t, 12.0, *state.TokenUsage.CostUSD, 1e-9,
		"session-wide cost spans both windows and counts each subagent token once")
	require.Equal(t, 8_000_000, state.TokenUsage.SubagentTokens.InputTokens)

	// Window 2 only: 2M main + the 3M the subagent grew after the reset = $5.00.
	require.NotNil(t, state.CheckpointTokenUsage)
	require.NotNil(t, state.CheckpointTokenUsage.CostUSD)
	require.InDelta(t, 5.0, *state.CheckpointTokenUsage.CostUSD, 1e-9,
		"the new window must not re-price subagent tokens the previous window already paid for")
	require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens)
	require.Equal(t, 3_000_000, state.CheckpointTokenUsage.SubagentTokens.InputTokens,
		"checkpoint window subagent delta = 8M cumulative less the 5M baseline")

	// The per-model mirror is reset with the window and must agree with it.
	perModel := state.ModelUsage["test-a"]
	require.NotNil(t, perModel)
	require.Equal(t, 5_000_000, perModel.InputTokens,
		"per-model window tokens = 2M main + 3M new subagent")
	require.NotNil(t, perModel.CostUSD)
	require.InDelta(t, 5.0, *perModel.CostUSD, 1e-9, "per-model window cost must match its token total")
}

// TestAccumulateTokenUsage_CarriesCacheCreation1hTokens pins the 1h-TTL cache
// subset through the accumulator. CacheCreation1hTokens is the subset of
// CacheCreationTokens billed at 2x input instead of 1.25x; accumulateTokenUsage
// dropped it on both its copy and its add path, so every accumulated total (and
// every re-priced display estimate derived from it) reported all-5-minute cache
// writes and undercharged.
func TestAccumulateTokenUsage_CarriesCacheCreation1hTokens(t *testing.T) {
	t.Parallel()

	step := &agent.TokenUsage{
		CacheCreationTokens:   1_000,
		CacheCreation1hTokens: 600,
		APICallCount:          1,
		SubagentTokens: &agent.TokenUsage{
			CacheCreationTokens:   500,
			CacheCreation1hTokens: 200,
			APICallCount:          1,
		},
	}

	// Copy path (existing == nil), including the deep-copied subagent subtree.
	acc := accumulateTokenUsage(nil, step)
	require.Equal(t, 600, acc.CacheCreation1hTokens, "copy path must carry the 1h subset")
	require.NotNil(t, acc.SubagentTokens)
	require.Equal(t, 200, acc.SubagentTokens.CacheCreation1hTokens,
		"the deep-copied subagent subtree must carry the 1h subset too")

	// Add path (existing != nil): the main-agent 1h subset sums like the rest.
	acc = accumulateTokenUsage(acc, step)
	require.Equal(t, 2_000, acc.CacheCreationTokens)
	require.Equal(t, 1_200, acc.CacheCreation1hTokens, "add path must sum the 1h subset")
}
