package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// mu builds a single-model bucket for terse test construction.
func mu(model string, u agent.TokenUsage) agent.ModelUsage {
	return agent.ModelUsage{Model: model, TokenUsage: u}
}

// accumulateModelUsage must fold buckets into a per-model map, unioning model
// keys across steps and summing token counts + costs for repeated models.
func TestAccumulateModelUsage_MultiStepUnionAndSum(t *testing.T) {
	t.Parallel()

	// Step 1: two models.
	m := accumulateModelUsage(nil, []agent.ModelUsage{
		mu("opus", agent.TokenUsage{InputTokens: 10, OutputTokens: 2, CostUSD: costPtr(0.5), CostSource: types.CostSourceEstimated}),
		mu("haiku", agent.TokenUsage{InputTokens: 4, CostUSD: costPtr(0.1), CostSource: types.CostSourceEstimated}),
	})
	// Step 2: opus again (must sum) plus a new model sonnet (union).
	m = accumulateModelUsage(m, []agent.ModelUsage{
		mu("opus", agent.TokenUsage{InputTokens: 5, OutputTokens: 3, CostUSD: costPtr(0.25), CostSource: types.CostSourceEstimated}),
		mu("sonnet", agent.TokenUsage{InputTokens: 7, CostUSD: costPtr(0.2), CostSource: types.CostSourceReported}),
	})

	if len(m) != 3 {
		t.Fatalf("model count = %d, want 3 (opus, haiku, sonnet)", len(m))
	}

	opus := m["opus"]
	if opus == nil {
		t.Fatal("opus bucket missing")
	}
	if opus.InputTokens != 15 || opus.OutputTokens != 5 {
		t.Fatalf("opus tokens = in %d out %d, want in 15 out 5", opus.InputTokens, opus.OutputTokens)
	}
	if opus.CostUSD == nil || *opus.CostUSD != 0.75 {
		t.Fatalf("opus cost = %v, want 0.75", opus.CostUSD)
	}
	if opus.CostSource != types.CostSourceEstimated {
		t.Fatalf("opus source = %q, want estimated", opus.CostSource)
	}
	if m["haiku"].InputTokens != 4 {
		t.Fatalf("haiku input = %d, want 4", m["haiku"].InputTokens)
	}
	if m["sonnet"].InputTokens != 7 || m["sonnet"].CostSource != types.CostSourceReported {
		t.Fatalf("sonnet bucket = %+v, want in 7 reported", *m["sonnet"])
	}
}

// A repeated model whose steps carry differing cost sources folds to mixed.
func TestAccumulateModelUsage_MixedCostSource(t *testing.T) {
	t.Parallel()
	m := accumulateModelUsage(nil, []agent.ModelUsage{
		mu("opus", agent.TokenUsage{InputTokens: 1, CostUSD: costPtr(1.0), CostSource: types.CostSourceReported}),
	})
	m = accumulateModelUsage(m, []agent.ModelUsage{
		mu("opus", agent.TokenUsage{InputTokens: 1, CostUSD: costPtr(0.5), CostSource: types.CostSourceEstimated}),
	})
	if m["opus"].CostUSD == nil || *m["opus"].CostUSD != 1.5 {
		t.Fatalf("opus cost = %v, want 1.5", m["opus"].CostUSD)
	}
	if m["opus"].CostSource != types.CostSourceMixed {
		t.Fatalf("opus source = %q, want mixed", m["opus"].CostSource)
	}
}

// The incoming buckets must not be aliased: mutating the accumulated map must not
// reach back into the caller's bucket slice.
func TestAccumulateModelUsage_CopiesBuckets(t *testing.T) {
	t.Parallel()
	incoming := []agent.ModelUsage{mu("opus", agent.TokenUsage{InputTokens: 10, CostUSD: costPtr(0.5)})}
	m := accumulateModelUsage(nil, incoming)
	m["opus"].InputTokens = 999
	if incoming[0].TokenUsage.InputTokens != 10 {
		t.Fatalf("mutating accumulated map mutated incoming bucket: %d", incoming[0].TokenUsage.InputTokens)
	}
	if m["opus"].CostUSD == incoming[0].TokenUsage.CostUSD {
		t.Fatal("accumulated cost pointer aliases incoming bucket")
	}
}

// Empty incoming leaves the map untouched (nil stays nil).
func TestAccumulateModelUsage_EmptyIncoming(t *testing.T) {
	t.Parallel()
	if got := accumulateModelUsage(nil, nil); got != nil {
		t.Fatalf("nil+nil = %v, want nil", got)
	}
	existing := map[string]*agent.TokenUsage{"opus": {InputTokens: 3}}
	if got := accumulateModelUsage(existing, nil); len(got) != 1 || got["opus"].InputTokens != 3 {
		t.Fatalf("existing+nil mutated map: %v", got)
	}
}

// Reset-on-checkpoint boundary: after condensation sets state.ModelUsage = nil,
// the next step must start a fresh map rather than re-adding to prior totals.
func TestAccumulateModelUsage_ResetBoundary(t *testing.T) {
	t.Parallel()
	m := accumulateModelUsage(nil, []agent.ModelUsage{mu("opus", agent.TokenUsage{InputTokens: 100})})
	if m["opus"].InputTokens != 100 {
		t.Fatalf("pre-reset opus = %d, want 100", m["opus"].InputTokens)
	}
	// Condensation boundary reset.
	m = nil
	// Next checkpoint window.
	m = accumulateModelUsage(m, []agent.ModelUsage{mu("opus", agent.TokenUsage{InputTokens: 7})})
	if m["opus"].InputTokens != 7 {
		t.Fatalf("post-reset opus = %d, want 7 (prior total must not carry over)", m["opus"].InputTokens)
	}
}

// sortedModelUsage flattens the map into a slice ordered by model for
// deterministic serialization; an empty map yields nil (omitempty drops it).
func TestSortedModelUsage_DeterministicOrder(t *testing.T) {
	t.Parallel()
	if got := sortedModelUsage(nil); got != nil {
		t.Fatalf("nil map = %v, want nil", got)
	}
	m := map[string]*agent.TokenUsage{
		"sonnet": {InputTokens: 1},
		"opus":   {InputTokens: 2},
		"haiku":  {InputTokens: 3},
	}
	got := sortedModelUsage(m)
	want := []string{"haiku", "opus", "sonnet"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, model := range want {
		if got[i].Model != model {
			t.Fatalf("got[%d].Model = %q, want %q", i, got[i].Model, model)
		}
	}
	// Value is a copy, not a shared pointer.
	got[1].TokenUsage.InputTokens = 999
	if m["opus"].InputTokens != 2 {
		t.Fatalf("sortedModelUsage aliased the map's pointer: %d", m["opus"].InputTokens)
	}
}
