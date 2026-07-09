package checkpoint

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func costPtr(v float64) *float64 { return &v }

// aggregateTokenUsage must sum CostUSD and merge CostSource across the two
// operands. (It intentionally does not recurse SubagentTokens — pre-existing
// behavior unchanged by the cost plumbing.)
func TestAggregateTokenUsage_SumsCost(t *testing.T) {
	t.Parallel()
	a := &agent.TokenUsage{InputTokens: 1, CostUSD: costPtr(0.75), CostSource: types.CostSourceReported}
	b := &agent.TokenUsage{InputTokens: 2, CostUSD: costPtr(0.25), CostSource: types.CostSourceReported}
	got := aggregateTokenUsage(a, b)
	if got.CostUSD == nil || *got.CostUSD != 1.0 {
		t.Fatalf("CostUSD = %v, want 1.0", got.CostUSD)
	}
	if got.CostSource != types.CostSourceReported {
		t.Fatalf("CostSource = %q, want reported", got.CostSource)
	}
}

func TestAggregateTokenUsage_MixedSource(t *testing.T) {
	t.Parallel()
	a := &agent.TokenUsage{CostUSD: costPtr(1), CostSource: types.CostSourceReported}
	b := &agent.TokenUsage{CostUSD: costPtr(1), CostSource: types.CostSourceEstimated}
	got := aggregateTokenUsage(a, b)
	if got.CostSource != types.CostSourceMixed {
		t.Fatalf("CostSource = %q, want mixed", got.CostSource)
	}
}

func TestAggregateTokenUsage_NilCostSideIgnoredForSource(t *testing.T) {
	t.Parallel()
	// b has an estimated label but nil cost, so it contributes no source.
	a := &agent.TokenUsage{CostUSD: costPtr(1), CostSource: types.CostSourceReported}
	b := &agent.TokenUsage{CostSource: types.CostSourceEstimated}
	got := aggregateTokenUsage(a, b)
	if got.CostUSD == nil || *got.CostUSD != 1 {
		t.Fatalf("CostUSD = %v, want 1", got.CostUSD)
	}
	if got.CostSource != types.CostSourceReported {
		t.Fatalf("CostSource = %q, want reported", got.CostSource)
	}
}

func TestAggregateTokenUsage_BothNilCost(t *testing.T) {
	t.Parallel()
	a := &agent.TokenUsage{InputTokens: 1}
	b := &agent.TokenUsage{InputTokens: 2}
	got := aggregateTokenUsage(a, b)
	if got.CostUSD != nil {
		t.Fatalf("CostUSD = %v, want nil", got.CostUSD)
	}
	if got.CostSource != "" {
		t.Fatalf("CostSource = %q, want empty", got.CostSource)
	}
}
