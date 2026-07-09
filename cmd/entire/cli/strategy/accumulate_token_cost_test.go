package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func costPtr(v float64) *float64 { return &v }

// accumulateTokenUsage must carry CostUSD/CostSource forward, copy (not alias)
// the cost pointer when existing is nil, and recurse cost into SubagentTokens.
func TestAccumulateTokenUsage_CarriesCost_ExistingNil(t *testing.T) {
	t.Parallel()
	incoming := &agent.TokenUsage{
		InputTokens: 10,
		CostUSD:     costPtr(0.5),
		CostSource:  types.CostSourceReported,
	}
	got := accumulateTokenUsage(nil, incoming)
	if got.CostUSD == nil || *got.CostUSD != 0.5 {
		t.Fatalf("CostUSD = %v, want 0.5", got.CostUSD)
	}
	if got.CostSource != types.CostSourceReported {
		t.Fatalf("CostSource = %q, want reported", got.CostSource)
	}
	// Must not alias incoming.CostUSD.
	if got.CostUSD == incoming.CostUSD {
		t.Fatal("result aliased incoming.CostUSD")
	}
	*got.CostUSD = 99
	if *incoming.CostUSD != 0.5 {
		t.Fatalf("mutating result mutated incoming: %v", *incoming.CostUSD)
	}
}

func TestAccumulateTokenUsage_SumsCost_MixedSource(t *testing.T) {
	t.Parallel()
	existing := &agent.TokenUsage{InputTokens: 5, CostUSD: costPtr(1.0), CostSource: types.CostSourceReported}
	incoming := &agent.TokenUsage{InputTokens: 3, CostUSD: costPtr(0.25), CostSource: types.CostSourceEstimated}
	got := accumulateTokenUsage(existing, incoming)
	if got.CostUSD == nil || *got.CostUSD != 1.25 {
		t.Fatalf("CostUSD = %v, want 1.25", got.CostUSD)
	}
	if got.CostSource != types.CostSourceMixed {
		t.Fatalf("CostSource = %q, want mixed", got.CostSource)
	}
}

func TestAccumulateTokenUsage_SubagentCostRecurses(t *testing.T) {
	t.Parallel()
	existing := &agent.TokenUsage{
		InputTokens:    5,
		SubagentTokens: &agent.TokenUsage{InputTokens: 1, CostUSD: costPtr(0.25), CostSource: types.CostSourceReported},
	}
	incoming := &agent.TokenUsage{
		InputTokens:    3,
		SubagentTokens: &agent.TokenUsage{InputTokens: 2, CostUSD: costPtr(0.5), CostSource: types.CostSourceReported},
	}
	got := accumulateTokenUsage(existing, incoming)
	if got.SubagentTokens == nil {
		t.Fatal("expected SubagentTokens carried")
	}
	if got.SubagentTokens.CostUSD == nil || *got.SubagentTokens.CostUSD != 0.75 {
		t.Fatalf("subagent CostUSD = %v, want 0.75", *got.SubagentTokens.CostUSD)
	}
	if got.SubagentTokens.CostSource != types.CostSourceReported {
		t.Fatalf("subagent CostSource = %q, want reported", got.SubagentTokens.CostSource)
	}
}
