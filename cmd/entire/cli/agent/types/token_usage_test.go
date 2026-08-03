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
