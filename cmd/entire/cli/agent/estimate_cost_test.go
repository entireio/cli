package agent

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// testTable (test-a: $1/MTok in+out, test-b: $2/MTok) is defined in
// token_usage_cost_test.go (same package).

// EstimateCost prices the persisted per-model buckets at current rates and folds
// them into a single estimated cost, ignoring any pre-existing cost on the input.
func TestEstimateCost_PricesModelBuckets(t *testing.T) {
	t.Parallel()
	models := []types.ModelUsage{
		{Model: "test-a", TokenUsage: types.TokenUsage{InputTokens: 400000, OutputTokens: 20000}},
	}
	cost, source := EstimateCost(nil, models, "", testTable(t))
	if cost == nil || *cost != 0.42 {
		t.Fatalf("cost = %v, want 0.42", cost)
	}
	if source != types.CostSourceEstimated {
		t.Fatalf("source = %q, want estimated", source)
	}
}

// A pre-existing cost on the buckets must be ignored: the estimate is recomputed
// from tokens at current rates (test-a => $0.42), not the stale $9.99.
func TestEstimateCost_IgnoresPreExistingCost(t *testing.T) {
	t.Parallel()
	stale := 9.99
	models := []types.ModelUsage{
		{Model: "test-a", TokenUsage: types.TokenUsage{InputTokens: 400000, OutputTokens: 20000, CostUSD: &stale, CostSource: types.CostSourceReported}},
	}
	cost, source := EstimateCost(nil, models, "", testTable(t))
	if cost == nil || *cost != 0.42 {
		t.Fatalf("cost = %v, want 0.42 (recomputed, not stale)", cost)
	}
	if source != types.CostSourceEstimated {
		t.Fatalf("source = %q, want estimated", source)
	}
}

// With no per-model buckets, EstimateCost prices the flat usage (subagents
// flattened in) under the fallback model.
func TestEstimateCost_FlatFallbackIncludesSubagents(t *testing.T) {
	t.Parallel()
	usage := &types.TokenUsage{
		InputTokens:  300000,
		OutputTokens: 20000,
		SubagentTokens: &types.TokenUsage{
			InputTokens: 100000,
		},
	}
	// (300000 + 20000 + 100000) tokens at $1/MTok = $0.42.
	cost, source := EstimateCost(usage, nil, "test-a", testTable(t))
	if cost == nil || *cost != 0.42 {
		t.Fatalf("cost = %v, want 0.42", cost)
	}
	if source != types.CostSourceEstimated {
		t.Fatalf("source = %q, want estimated", source)
	}
}

// A nil table means estimation is disabled: no cost, never $0.
func TestEstimateCost_NilTableNoCost(t *testing.T) {
	t.Parallel()
	models := []types.ModelUsage{{Model: "test-a", TokenUsage: types.TokenUsage{InputTokens: 400000}}}
	cost, source := EstimateCost(nil, models, "", nil)
	if cost != nil {
		t.Fatalf("cost = %v, want nil for nil table", cost)
	}
	if source != "" {
		t.Fatalf("source = %q, want empty", source)
	}
}

// An unknown model is unpriceable: no cost (never $0).
func TestEstimateCost_UnknownModelNoCost(t *testing.T) {
	t.Parallel()
	models := []types.ModelUsage{{Model: "who-dis", TokenUsage: types.TokenUsage{InputTokens: 400000}}}
	cost, source := EstimateCost(nil, models, "", testTable(t))
	if cost != nil {
		t.Fatalf("cost = %v, want nil for unknown model", cost)
	}
	if source != "" {
		t.Fatalf("source = %q, want empty", source)
	}
}

// A priceable bucket alongside an unpriceable-but-token-bearing bucket folds to
// mixed (partial coverage): the estimate covers only the priceable tokens.
func TestEstimateCost_PartialCoverageMixed(t *testing.T) {
	t.Parallel()
	models := []types.ModelUsage{
		{Model: "test-a", TokenUsage: types.TokenUsage{InputTokens: 400000, OutputTokens: 20000}},
		{Model: "who-dis", TokenUsage: types.TokenUsage{InputTokens: 5000}},
	}
	cost, source := EstimateCost(nil, models, "", testTable(t))
	if cost == nil || *cost != 0.42 {
		t.Fatalf("cost = %v, want 0.42 (only the priceable bucket)", cost)
	}
	if source != types.CostSourceMixed {
		t.Fatalf("source = %q, want mixed", source)
	}
}
