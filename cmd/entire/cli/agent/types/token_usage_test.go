package types

import "testing"

func TestAddTokenUsage(t *testing.T) {
	t.Parallel()

	if got := AddTokenUsage(nil, nil); got != nil {
		t.Errorf("AddTokenUsage(nil, nil) = %+v, want nil", got)
	}

	only := &TokenUsage{InputTokens: 3}
	if got := AddTokenUsage(nil, only); got == nil || got.InputTokens != 3 {
		t.Errorf("AddTokenUsage(nil, x) = %+v, want a copy of x", got)
	}
	if got := AddTokenUsage(only, nil); got == only {
		t.Error("AddTokenUsage must not return an input pointer (would alias caller state)")
	}

	a := &TokenUsage{InputTokens: 1, OutputTokens: 2, APICallCount: 1, SubagentTokens: &TokenUsage{InputTokens: 10}}
	b := &TokenUsage{InputTokens: 4, OutputTokens: 5, APICallCount: 2, SubagentTokens: &TokenUsage{InputTokens: 20}}
	got := AddTokenUsage(a, b)
	if got.InputTokens != 5 || got.OutputTokens != 7 || got.APICallCount != 3 {
		t.Errorf("top-level sum = %+v", got)
	}
	if got.SubagentTokens == nil || got.SubagentTokens.InputTokens != 30 {
		t.Errorf("subagent sum = %+v, want InputTokens 30", got.SubagentTokens)
	}
	if a.InputTokens != 1 || a.SubagentTokens.InputTokens != 10 {
		t.Error("AddTokenUsage mutated an input")
	}
}

// TestAddTokenUsage_TruncatesDeepSubagentChains pins MaxSubagentDepth. Token usage
// is read back from per-session metadata.json blobs on the shared checkpoint
// branch, so the chain depth is not trustworthy; an unbounded chain reaching the
// root CheckpointSummary is a write-amplification vector, because that summary is
// re-marshalled with indentation (O(depth²) in output size).
func TestAddTokenUsage_TruncatesDeepSubagentChains(t *testing.T) {
	t.Parallel()

	// Build a chain far deeper than any real agent reports (real chains are depth 1).
	deep := &TokenUsage{InputTokens: 1}
	for range MaxSubagentDepth * 3 {
		deep = &TokenUsage{InputTokens: 1, SubagentTokens: deep}
	}

	depth := 0
	for got := AddTokenUsage(deep, deep); got != nil; got = got.SubagentTokens {
		depth++
		if depth > MaxSubagentDepth*2 {
			t.Fatalf("chain not truncated: walked %d levels", depth)
		}
	}
	if depth != MaxSubagentDepth+1 {
		t.Errorf("result depth = %d, want %d (MaxSubagentDepth + the top level)", depth, MaxSubagentDepth+1)
	}
}

// TestAddTokenUsage_KeepsRealDepthIntact is the companion guard: the cap must not
// clip the depth-1 chains agents actually produce.
func TestAddTokenUsage_KeepsRealDepthIntact(t *testing.T) {
	t.Parallel()

	got := AddTokenUsage(
		&TokenUsage{InputTokens: 1, SubagentTokens: &TokenUsage{InputTokens: 10}},
		&TokenUsage{InputTokens: 2, SubagentTokens: &TokenUsage{InputTokens: 20}},
	)
	if got.SubagentTokens == nil || got.SubagentTokens.InputTokens != 30 {
		t.Fatalf("subagent total = %+v, want InputTokens 30", got.SubagentTokens)
	}
	if got.SubagentTokens.SubagentTokens != nil {
		t.Error("must not synthesize a nested level that the inputs did not have")
	}
}

// ThinkingTokens and CacheCreation1hTokens are subsets of OutputTokens and
// CacheCreationTokens; they are still added (and subtracted) at every nesting
// level, otherwise the root CheckpointSummary sum silently drops them — the same
// way SubagentTokens once went missing from a field-by-field copy.
func TestAddTokenUsage_SumsSubsetFieldsAtEveryDepth(t *testing.T) {
	t.Parallel()

	a := &TokenUsage{OutputTokens: 100, ThinkingTokens: 40, CacheCreationTokens: 50, CacheCreation1hTokens: 50,
		SubagentTokens: &TokenUsage{OutputTokens: 10, ThinkingTokens: 4, CacheCreationTokens: 5, CacheCreation1hTokens: 5}}
	b := &TokenUsage{OutputTokens: 200, ThinkingTokens: 60, CacheCreationTokens: 30, CacheCreation1hTokens: 0,
		SubagentTokens: &TokenUsage{OutputTokens: 20, ThinkingTokens: 6, CacheCreationTokens: 3, CacheCreation1hTokens: 3}}
	got := AddTokenUsage(a, b)
	if got.ThinkingTokens != 100 || got.CacheCreation1hTokens != 50 {
		t.Errorf("top-level subsets = thinking %d (want 100), 1h %d (want 50)", got.ThinkingTokens, got.CacheCreation1hTokens)
	}
	if got.SubagentTokens == nil || got.SubagentTokens.ThinkingTokens != 10 || got.SubagentTokens.CacheCreation1hTokens != 8 {
		t.Errorf("subagent subsets = %+v, want thinking 10, 1h 8", got.SubagentTokens)
	}

	diff := SubtractTokenUsage(got, a)
	if diff.ThinkingTokens != 60 || diff.CacheCreation1hTokens != 0 || diff.SubagentTokens.ThinkingTokens != 6 {
		t.Errorf("SubtractTokenUsage subsets = %+v / %+v", diff, diff.SubagentTokens)
	}
}

// Model on a subagent entry lets cost be weighted per model. Sums of entries on
// the same model keep it; sums across different models clear it (mixed), and a
// side with no model never overrides one that has it.
func TestAddTokenUsage_ModelKeptWhenSameClearedWhenMixed(t *testing.T) {
	t.Parallel()

	same := AddTokenUsage(&TokenUsage{OutputTokens: 1, Model: "claude-haiku-4-5"}, &TokenUsage{OutputTokens: 2, Model: "claude-haiku-4-5"})
	if same.Model != "claude-haiku-4-5" {
		t.Errorf("same model should be kept, got %q", same.Model)
	}
	mixed := AddTokenUsage(&TokenUsage{OutputTokens: 1, Model: "claude-haiku-4-5"}, &TokenUsage{OutputTokens: 2, Model: "claude-fable-5"})
	if mixed.Model != "" {
		t.Errorf("mixed models should clear Model, got %q", mixed.Model)
	}
	oneSided := AddTokenUsage(&TokenUsage{OutputTokens: 1, Model: "claude-haiku-4-5"}, &TokenUsage{OutputTokens: 2})
	if oneSided.Model != "claude-haiku-4-5" {
		t.Errorf("a side without a model must not clear the other's, got %q", oneSided.Model)
	}
	if got := AddTokenUsage(nil, &TokenUsage{Model: "m"}); got.Model != "m" {
		t.Errorf("copy of one operand must keep Model, got %q", got.Model)
	}
}
