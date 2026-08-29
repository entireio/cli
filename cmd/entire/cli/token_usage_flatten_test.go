package cli

import (
	"math"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func TestFlattenTokenUsage_CarriesSubsetFieldsAcrossLevels(t *testing.T) {
	t.Parallel()

	u := &agent.TokenUsage{
		OutputTokens: 100, ThinkingTokens: 40, CacheCreationTokens: 50, CacheCreation1hTokens: 50, APICallCount: 3,
		SubagentTokens: &agent.TokenUsage{
			OutputTokens: 10, ThinkingTokens: 4, CacheCreationTokens: 5, CacheCreation1hTokens: 0, APICallCount: 2, Model: "claude-haiku-4-5",
		},
	}
	flat := flattenTokenUsage(u)
	if flat.OutputTokens != 110 || flat.ThinkingTokens != 44 || flat.CacheCreationTokens != 55 || flat.CacheCreation1hTokens != 50 || flat.APICallCount != 5 {
		t.Errorf("flatten = %+v", flat)
	}
	if flat.SubagentTokens != nil {
		t.Error("flattened usage must have no nested chain")
	}
	if u.SubagentTokens == nil || u.OutputTokens != 100 {
		t.Error("input mutated")
	}
}

func TestFlattenTokenUsage_NilAndLeaf(t *testing.T) {
	t.Parallel()

	if got := flattenTokenUsage(nil); got != nil {
		t.Errorf("flattenTokenUsage(nil) = %+v, want nil", got)
	}

	leaf := &agent.TokenUsage{InputTokens: 7, Model: "claude-sonnet-4-5"}
	got := flattenTokenUsage(leaf)
	if got == leaf {
		t.Error("flattenTokenUsage must not return the input pointer (would alias caller state)")
	}
	if got == nil || got.InputTokens != 7 || got.Model != "claude-sonnet-4-5" || got.SubagentTokens != nil {
		t.Errorf("flattenTokenUsage(leaf) = %+v, want a detached copy", got)
	}
}

// TestFlattenTokenUsage_SaturatesOverflow replaced the same pin on the retired
// addCheckpointTokenUsage: a corrupt or hostile blob near math.MaxInt must
// saturate on every field, including across the nested chain, never wrap.
func TestFlattenTokenUsage_SaturatesOverflow(t *testing.T) {
	t.Parallel()

	flat := flattenTokenUsage(&agent.TokenUsage{
		InputTokens:           math.MaxInt,
		CacheCreationTokens:   math.MaxInt,
		CacheReadTokens:       math.MaxInt,
		OutputTokens:          math.MaxInt,
		APICallCount:          math.MaxInt,
		ThinkingTokens:        math.MaxInt,
		CacheCreation1hTokens: math.MaxInt,
		SubagentTokens: &agent.TokenUsage{
			InputTokens:           1,
			CacheCreationTokens:   1,
			CacheReadTokens:       1,
			OutputTokens:          1,
			APICallCount:          1,
			ThinkingTokens:        1,
			CacheCreation1hTokens: 1,
			SubagentTokens:        &agent.TokenUsage{InputTokens: 1},
		},
	})

	if flat.InputTokens != math.MaxInt ||
		flat.CacheCreationTokens != math.MaxInt ||
		flat.CacheReadTokens != math.MaxInt ||
		flat.OutputTokens != math.MaxInt ||
		flat.APICallCount != math.MaxInt ||
		flat.ThinkingTokens != math.MaxInt ||
		flat.CacheCreation1hTokens != math.MaxInt {
		t.Fatalf("expected saturated flattened usage, got %+v", flat)
	}
	if flat.SubagentTokens != nil {
		t.Error("flattened usage must have no nested chain")
	}
}

// TestFlattenTokenUsage_TruncatesDeepChains mirrors the AddTokenUsage cap: the
// top level plus MaxSubagentDepth nested levels are summed, anything deeper is
// dropped rather than walked.
func TestFlattenTokenUsage_TruncatesDeepChains(t *testing.T) {
	t.Parallel()

	deep := &agent.TokenUsage{InputTokens: 1}
	for range types.MaxSubagentDepth * 3 {
		deep = &agent.TokenUsage{InputTokens: 1, SubagentTokens: deep}
	}

	got := flattenTokenUsage(deep)
	if want := types.MaxSubagentDepth + 1; got.InputTokens != want {
		t.Errorf("flattened InputTokens = %d, want %d (top level + MaxSubagentDepth)", got.InputTokens, want)
	}
}

func TestFlattenTokenUsage_ModelKeptWhenSameClearedWhenMixed(t *testing.T) {
	t.Parallel()

	same := flattenTokenUsage(&agent.TokenUsage{
		Model:          "claude-opus-4-1",
		SubagentTokens: &agent.TokenUsage{Model: "claude-opus-4-1"},
	})
	if same.Model != "claude-opus-4-1" {
		t.Errorf("same model across levels: Model = %q, want kept", same.Model)
	}

	mixed := flattenTokenUsage(&agent.TokenUsage{
		Model:          "claude-opus-4-1",
		SubagentTokens: &agent.TokenUsage{Model: "claude-haiku-4-5"},
	})
	if mixed.Model != "" {
		t.Errorf("mixed models across levels: Model = %q, want cleared", mixed.Model)
	}
}
