package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

func fptr(v float64) *float64 { return &v }

// TestWriteCommitted_PersistsCost is the CLI-side cost-ownership invariant: a
// checkpoint write persists the cost computed when the tokens were spent, at
// every level (flat, subagent subtree, per-model bucket) in both the session
// metadata and the aggregated root summary, alongside the token breakdown.
//
// Pricing once, client-side, at spend time is both simpler and more historically
// accurate than having a backend re-derive cost later from raw token counts:
// only the CLI knows which pricing was in force for this session, a backend
// would need a full historical pricing table to reproduce it, and repo lineage
// (needed to pick the right table) is absent for forked/renamed repos.
func TestWriteCommitted_PersistsCost(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("c05700000001")

	err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "cost-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"message":"x"}`)),
		Model:        "claude-opus-4-8",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         1000,
			OutputTokens:        200,
			CacheReadTokens:     300,
			CacheCreationTokens: 40,
			APICallCount:        5,
			CostUSD:             fptr(1.23),
			CostSource:          types.CostSourceEstimated,
			SubagentTokens: &agent.TokenUsage{
				InputTokens:  10,
				OutputTokens: 2,
				CostUSD:      fptr(0.05),
				CostSource:   types.CostSourceReported,
			},
		},
		ModelUsage: []types.ModelUsage{
			{Model: "claude-opus-4-8", TokenUsage: agent.TokenUsage{
				InputTokens:         1000,
				OutputTokens:        200,
				CacheReadTokens:     300,
				CacheCreationTokens: 40,
				CostUSD:             fptr(1.23),
				CostSource:          types.CostSourceEstimated,
			}},
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	meta, err := store.ReadSessionMetadata(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionMetadata() error = %v", err)
	}
	assertUsageTokensAndCost(t, "session flat", meta.TokenUsage, 1000, 200, 300, 40, 1.23, types.CostSourceEstimated)
	if meta.TokenUsage.APICallCount != 5 {
		t.Errorf("session flat APICallCount = %d, want 5", meta.TokenUsage.APICallCount)
	}

	// Subagent subtree: token counts AND its own cost provenance preserved.
	if meta.TokenUsage.SubagentTokens == nil {
		t.Fatal("subagent tokens must be persisted")
	}
	assertUsageTokensAndCost(t, "session subagent", meta.TokenUsage.SubagentTokens, 10, 2, 0, 0, 0.05, types.CostSourceReported)

	// Per-model breakdown: model id, four token fields, and cost preserved.
	if len(meta.ModelUsage) != 1 {
		t.Fatalf("len(meta.ModelUsage) = %d, want 1", len(meta.ModelUsage))
	}
	if meta.ModelUsage[0].Model != "claude-opus-4-8" {
		t.Errorf("model id = %q, want claude-opus-4-8", meta.ModelUsage[0].Model)
	}
	mu := meta.ModelUsage[0].TokenUsage
	assertUsageTokensAndCost(t, "session per-model", &mu, 1000, 200, 300, 40, 1.23, types.CostSourceEstimated)

	// Aggregated root summary: token counts and cost preserved.
	summary, err := store.Read(context.Background(), cpID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if summary == nil || summary.TokenUsage == nil {
		t.Fatal("summary token usage must be persisted")
	}
	assertUsageTokensAndCost(t, "summary flat", summary.TokenUsage, 1000, 200, 300, 40, 1.23, types.CostSourceEstimated)
	if len(summary.ModelUsage) == 0 {
		t.Fatal("summary per-model breakdown must be persisted")
	}
	for i := range summary.ModelUsage {
		if summary.ModelUsage[i].Model == "" {
			t.Errorf("summary per-model bucket %d has empty model id", i)
		}
		b := summary.ModelUsage[i].TokenUsage
		if b.CostUSD == nil || b.CostSource == "" {
			t.Errorf("summary per-model cost must be persisted, got %+v", b)
		}
	}
}

// A checkpoint written with cost must still carry that cost after the metadata is
// rewritten in place by the attribution and summary backfills. These rewrite
// existing blobs (including blobs an older CLI wrote), so a stripping step here
// would silently erase cost from already-committed checkpoints.
func TestBackfills_PreserveCost(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("c05700000002")
	ctx := context.Background()

	if err := store.Write(ctx, Session{
		CheckpointID: cpID,
		SessionID:    "backfill-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"message":"x"}`)),
		Model:        "claude-opus-4-8",
		TokenUsage: &agent.TokenUsage{
			InputTokens: 1000,
			CostUSD:     fptr(1.23),
			CostSource:  types.CostSourceEstimated,
		},
		ModelUsage: []types.ModelUsage{
			{Model: "claude-opus-4-8", TokenUsage: agent.TokenUsage{
				InputTokens: 1000,
				CostUSD:     fptr(1.23),
				CostSource:  types.CostSourceEstimated,
			}},
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	}); err != nil {
		t.Fatalf("Write(Session) error = %v", err)
	}

	// Root-summary rewrite.
	if err := store.Write(ctx, CheckpointAttribution{
		CheckpointID: cpID,
		Attribution:  &Attribution{AgentLines: 7, TotalLinesChanged: 7, AgentPercentage: 100},
	}); err != nil {
		t.Fatalf("Write(CheckpointAttribution) error = %v", err)
	}
	// Session-metadata rewrite.
	if err := store.Write(ctx, SessionSummary{
		CheckpointID: cpID,
		Summary:      &Summary{Intent: "i", Outcome: "o"},
	}); err != nil {
		t.Fatalf("Write(SessionSummary) error = %v", err)
	}

	meta, err := store.ReadSessionMetadata(ctx, cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionMetadata() error = %v", err)
	}
	if meta.Summary == nil {
		t.Fatal("summary backfill did not land")
	}
	assertCostPresent(t, "session flat after summary backfill", meta.TokenUsage, 1.23, types.CostSourceEstimated)
	if len(meta.ModelUsage) != 1 {
		t.Fatalf("len(meta.ModelUsage) = %d, want 1", len(meta.ModelUsage))
	}
	mu := meta.ModelUsage[0].TokenUsage
	assertCostPresent(t, "session per-model after summary backfill", &mu, 1.23, types.CostSourceEstimated)

	summary, err := store.Read(ctx, cpID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if summary.CombinedAttribution == nil {
		t.Fatal("attribution backfill did not land")
	}
	assertCostPresent(t, "summary flat after attribution backfill", summary.TokenUsage, 1.23, types.CostSourceEstimated)
	if len(summary.ModelUsage) == 0 {
		t.Fatal("summary per-model breakdown must survive the backfill")
	}
	for i := range summary.ModelUsage {
		b := summary.ModelUsage[i].TokenUsage
		assertCostPresent(t, "summary per-model after attribution backfill", &b, 1.23, types.CostSourceEstimated)
	}
}

func assertUsageTokensAndCost(t *testing.T, label string, u *agent.TokenUsage, input, output, cacheRead, cacheWrite int, cost float64, source string) {
	t.Helper()
	if u == nil {
		t.Fatalf("%s: usage must be persisted", label)
	}
	if u.InputTokens != input || u.OutputTokens != output || u.CacheReadTokens != cacheRead || u.CacheCreationTokens != cacheWrite {
		t.Errorf("%s: token counts = in=%d out=%d cr=%d cw=%d, want in=%d out=%d cr=%d cw=%d",
			label, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens, input, output, cacheRead, cacheWrite)
	}
	assertCostPresent(t, label, u, cost, source)
}

func assertCostPresent(t *testing.T, label string, u *agent.TokenUsage, cost float64, source string) {
	t.Helper()
	if u == nil {
		t.Fatalf("%s: usage must be persisted", label)
	}
	if u.CostUSD == nil {
		t.Errorf("%s: cost_usd must be persisted, got nil", label)
	} else if *u.CostUSD != cost {
		t.Errorf("%s: cost_usd = %v, want %v", label, *u.CostUSD, cost)
	}
	if u.CostSource != source {
		t.Errorf("%s: cost_source = %q, want %q", label, u.CostSource, source)
	}
}
