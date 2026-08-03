package types

// TokenUsage represents aggregated token usage for a checkpoint.
// This is agent-agnostic and can be populated by any agent that tracks token usage.
type TokenUsage struct {
	// InputTokens is the number of input tokens (fresh, not from cache)
	InputTokens int `json:"input_tokens"`
	// CacheCreationTokens is tokens written to cache (billable at cache write rate)
	CacheCreationTokens int `json:"cache_creation_tokens"`
	// CacheReadTokens is tokens read from cache (discounted rate)
	CacheReadTokens int `json:"cache_read_tokens"`
	// OutputTokens is the number of output tokens generated
	OutputTokens int `json:"output_tokens"`
	// APICallCount is the number of API calls made
	APICallCount int `json:"api_call_count"`
	// SubagentTokens contains token usage from spawned subagents (if any)
	SubagentTokens *TokenUsage `json:"subagent_tokens,omitempty"`
}

// AddTokenUsage returns the sum of a and b, recursing into subagent usage.
// Either operand may be nil (treated as zero); the result is nil only when both
// are. Neither input is mutated.
func AddTokenUsage(a, b *TokenUsage) *TokenUsage {
	if a == nil && b == nil {
		return nil
	}
	sum := &TokenUsage{}
	var aSub, bSub *TokenUsage
	if a != nil {
		sum.InputTokens = a.InputTokens
		sum.CacheCreationTokens = a.CacheCreationTokens
		sum.CacheReadTokens = a.CacheReadTokens
		sum.OutputTokens = a.OutputTokens
		sum.APICallCount = a.APICallCount
		aSub = a.SubagentTokens
	}
	if b != nil {
		sum.InputTokens += b.InputTokens
		sum.CacheCreationTokens += b.CacheCreationTokens
		sum.CacheReadTokens += b.CacheReadTokens
		sum.OutputTokens += b.OutputTokens
		sum.APICallCount += b.APICallCount
		bSub = b.SubagentTokens
	}
	sum.SubagentTokens = AddTokenUsage(aSub, bSub)
	return sum
}

// SubtractTokenUsage returns a-b, recursing into subagent usage and clamping
// every field at zero (a nil operand is treated as zero). Neither input is
// mutated. Used to rescope a cumulative-since-session-start snapshot (e.g.
// subagent token usage, which is always re-read from the start of each
// subagent transcript) down to a delta since a previously captured baseline.
func SubtractTokenUsage(a, b *TokenUsage) *TokenUsage {
	if a == nil {
		return nil
	}
	if b == nil {
		b = &TokenUsage{}
	}
	diff := &TokenUsage{
		InputTokens:         clampSubtract(a.InputTokens, b.InputTokens),
		CacheCreationTokens: clampSubtract(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheReadTokens:     clampSubtract(a.CacheReadTokens, b.CacheReadTokens),
		OutputTokens:        clampSubtract(a.OutputTokens, b.OutputTokens),
		APICallCount:        clampSubtract(a.APICallCount, b.APICallCount),
	}
	diff.SubagentTokens = SubtractTokenUsage(a.SubagentTokens, b.SubagentTokens)
	return diff
}

// clampSubtract returns a-b, floored at zero so a stale or racy baseline
// never produces a negative delta.
func clampSubtract(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}
