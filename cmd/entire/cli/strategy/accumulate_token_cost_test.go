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

// Two independent aggregates (mirroring state.TokenUsage and
// state.CheckpointTokenUsage in manual_commit_git) that fold the SAME step must
// not share the step's SubagentTokens object. Otherwise a later in-place
// accumulation mutates the shared subtree and cross-contaminates both aggregates,
// inflating subagent counts. Two steps of 100 subagent tokens must land each
// aggregate at exactly 200.
func TestAccumulateTokenUsage_SubagentNoSharedPointerAcrossAggregates(t *testing.T) {
	t.Parallel()
	step := &agent.TokenUsage{
		InputTokens:    10,
		SubagentTokens: &agent.TokenUsage{InputTokens: 100},
	}

	var aggA, aggB *agent.TokenUsage
	// Step 1: seed both aggregates from the same step.
	aggA = accumulateTokenUsage(aggA, step)
	aggB = accumulateTokenUsage(aggB, step)
	// Step 2: fold the same step again into each.
	aggA = accumulateTokenUsage(aggA, step)
	aggB = accumulateTokenUsage(aggB, step)

	if aggA.SubagentTokens.InputTokens != 200 {
		t.Fatalf("aggA subagent input = %d, want 200", aggA.SubagentTokens.InputTokens)
	}
	if aggB.SubagentTokens.InputTokens != 200 {
		t.Fatalf("aggB subagent input = %d, want 200", aggB.SubagentTokens.InputTokens)
	}
	// The incoming step must never be mutated by accumulation.
	if step.SubagentTokens.InputTokens != 100 {
		t.Fatalf("step subagent mutated: %d, want 100", step.SubagentTokens.InputTokens)
	}
	// The two aggregates must not share the same SubagentTokens object.
	if aggA.SubagentTokens == aggB.SubagentTokens {
		t.Fatal("aggregates share the same SubagentTokens pointer")
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
