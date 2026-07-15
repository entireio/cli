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

// TestWriteCommitted_DoesNotPersistCost is the CLI-side cost-ownership invariant:
// a checkpoint write must persist the full token breakdown the platform prices
// from (flat counts, subagent subtree, and per-model buckets with the four token
// fields + model id) but MUST NOT persist any cost — cost_usd/cost_source are
// nil/empty at every level (flat, subagent, per-model) in both the session
// metadata and the aggregated root summary, even when the write request carries
// cost. entire-api owns server-side pricing from these tokens.
func TestWriteCommitted_DoesNotPersistCost(t *testing.T) {
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
	assertUsageTokensNoCost(t, "session flat", meta.TokenUsage, 1000, 200, 300, 40)
	if meta.TokenUsage.APICallCount != 5 {
		t.Errorf("session flat APICallCount = %d, want 5", meta.TokenUsage.APICallCount)
	}

	// Subagent subtree: token counts preserved, cost stripped.
	if meta.TokenUsage.SubagentTokens == nil {
		t.Fatal("subagent tokens must be persisted")
	}
	assertUsageTokensNoCost(t, "session subagent", meta.TokenUsage.SubagentTokens, 10, 2, 0, 0)

	// Per-model breakdown: model id + four token fields preserved, cost stripped.
	if len(meta.ModelUsage) != 1 {
		t.Fatalf("len(meta.ModelUsage) = %d, want 1", len(meta.ModelUsage))
	}
	if meta.ModelUsage[0].Model != "claude-opus-4-8" {
		t.Errorf("model id = %q, want claude-opus-4-8", meta.ModelUsage[0].Model)
	}
	mu := meta.ModelUsage[0].TokenUsage
	assertUsageTokensNoCost(t, "session per-model", &mu, 1000, 200, 300, 40)

	// Aggregated root summary: token counts preserved, cost stripped.
	summary, err := store.Read(context.Background(), cpID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if summary == nil || summary.TokenUsage == nil {
		t.Fatal("summary token usage must be persisted")
	}
	assertUsageTokensNoCost(t, "summary flat", summary.TokenUsage, 1000, 200, 300, 40)
	for i := range summary.ModelUsage {
		if summary.ModelUsage[i].Model == "" {
			t.Errorf("summary per-model bucket %d has empty model id", i)
		}
		b := summary.ModelUsage[i].TokenUsage
		if b.CostUSD != nil || b.CostSource != "" {
			t.Errorf("summary per-model cost must not be persisted: %+v", b)
		}
	}
}

func assertUsageTokensNoCost(t *testing.T, label string, u *agent.TokenUsage, input, output, cacheRead, cacheWrite int) {
	t.Helper()
	if u == nil {
		t.Fatalf("%s: usage must be persisted", label)
	}
	if u.InputTokens != input || u.OutputTokens != output || u.CacheReadTokens != cacheRead || u.CacheCreationTokens != cacheWrite {
		t.Errorf("%s: token counts = in=%d out=%d cr=%d cw=%d, want in=%d out=%d cr=%d cw=%d",
			label, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens, input, output, cacheRead, cacheWrite)
	}
	if u.CostUSD != nil {
		t.Errorf("%s: cost_usd must NOT be persisted, got %v", label, *u.CostUSD)
	}
	if u.CostSource != "" {
		t.Errorf("%s: cost_source must NOT be persisted, got %q", label, u.CostSource)
	}
}
