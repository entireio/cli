package checkpoint

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// mergeModelUsage is the per-model aggregation primitive behind the root
// CheckpointSummary. Two sessions with an overlapping model must produce a single
// merged bucket (tokens summed, costs folded) plus the union of distinct models,
// sorted by model for deterministic output.
func TestMergeModelUsage_OverlappingModelsMerge(t *testing.T) {
	t.Parallel()

	// Session 0's per-model list.
	sessionA := []types.ModelUsage{
		{Model: "opus", TokenUsage: types.TokenUsage{InputTokens: 10, OutputTokens: 2, CostUSD: costPtr(0.5), CostSource: types.CostSourceEstimated}},
		{Model: "haiku", TokenUsage: types.TokenUsage{InputTokens: 4, CostUSD: costPtr(0.1), CostSource: types.CostSourceEstimated}},
	}
	// Session 1's per-model list: opus overlaps, sonnet is new.
	sessionB := []types.ModelUsage{
		{Model: "opus", TokenUsage: types.TokenUsage{InputTokens: 6, OutputTokens: 1, CostUSD: costPtr(0.25), CostSource: types.CostSourceEstimated}},
		{Model: "sonnet", TokenUsage: types.TokenUsage{InputTokens: 8, CostUSD: costPtr(0.2), CostSource: types.CostSourceReported}},
	}

	got := mergeModelUsage(sessionA, sessionB)

	// Deterministic order: haiku, opus, sonnet.
	wantOrder := []string{"haiku", "opus", "sonnet"}
	if len(got) != len(wantOrder) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(wantOrder), got)
	}
	for i, model := range wantOrder {
		if got[i].Model != model {
			t.Fatalf("got[%d].Model = %q, want %q", i, got[i].Model, model)
		}
	}

	// opus is the merged bucket.
	opus := got[1].TokenUsage
	if opus.InputTokens != 16 || opus.OutputTokens != 3 {
		t.Fatalf("opus tokens = in %d out %d, want in 16 out 3", opus.InputTokens, opus.OutputTokens)
	}
	if opus.CostUSD == nil || *opus.CostUSD != 0.75 {
		t.Fatalf("opus cost = %v, want 0.75", opus.CostUSD)
	}
	if opus.CostSource != types.CostSourceEstimated {
		t.Fatalf("opus source = %q, want estimated", opus.CostSource)
	}
}

// A model priced by one session (reported) but not the other folds to mixed only
// when both sides carry cost; a nil-cost side contributes no source label.
func TestMergeModelUsage_MixedAndNilCostSource(t *testing.T) {
	t.Parallel()

	a := []types.ModelUsage{{Model: "opus", TokenUsage: types.TokenUsage{InputTokens: 1, CostUSD: costPtr(1.0), CostSource: types.CostSourceReported}}}
	// Same model, estimated cost -> mixed.
	b := []types.ModelUsage{{Model: "opus", TokenUsage: types.TokenUsage{InputTokens: 1, CostUSD: costPtr(0.5), CostSource: types.CostSourceEstimated}}}
	got := mergeModelUsage(a, b)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].TokenUsage.CostSource != types.CostSourceMixed {
		t.Fatalf("source = %q, want mixed", got[0].TokenUsage.CostSource)
	}
	if *got[0].TokenUsage.CostUSD != 1.5 {
		t.Fatalf("cost = %v, want 1.5", got[0].TokenUsage.CostUSD)
	}
}

// Empty inputs yield nil so the omitempty JSON tag drops model_usage entirely.
func TestMergeModelUsage_EmptyYieldsNil(t *testing.T) {
	t.Parallel()
	if got := mergeModelUsage(nil, nil); got != nil {
		t.Fatalf("nil+nil = %v, want nil", got)
	}
	// One empty side just returns the other, sorted.
	only := []types.ModelUsage{{Model: "opus", TokenUsage: types.TokenUsage{InputTokens: 3}}}
	got := mergeModelUsage(nil, only)
	if len(got) != 1 || got[0].Model != "opus" || got[0].TokenUsage.InputTokens != 3 {
		t.Fatalf("nil+one = %v, want single opus", got)
	}
}

// The merge must not alias the input buckets' cost pointers.
func TestMergeModelUsage_NoAliasing(t *testing.T) {
	t.Parallel()
	in := []types.ModelUsage{{Model: "opus", TokenUsage: types.TokenUsage{InputTokens: 5, CostUSD: costPtr(0.5)}}}
	got := mergeModelUsage(in, nil)
	if got[0].TokenUsage.CostUSD == in[0].TokenUsage.CostUSD {
		t.Fatal("merged cost pointer aliases input bucket")
	}
	got[0].TokenUsage.InputTokens = 999
	if in[0].TokenUsage.InputTokens != 5 {
		t.Fatalf("mutating merged result mutated input: %d", in[0].TokenUsage.InputTokens)
	}
}
