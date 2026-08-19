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
