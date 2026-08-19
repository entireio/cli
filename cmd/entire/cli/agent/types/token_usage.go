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

// MaxSubagentDepth caps how deep a SubagentTokens chain is walked. Real chains
// are depth 1 — an agent reports one aggregate for all its subagents — so this is
// insurance against a malformed or hostile chain, not a real limit.
//
// It matters because token usage is read back from per-session metadata.json blobs
// on the shared checkpoint branch, which anyone with push access can author. The
// depth is not a stack-overflow risk (encoding/json caps nesting at 10000), but an
// unbounded chain reaching the root CheckpointSummary is a write amplification
// vector: the summary is re-marshalled with indentation, which is O(depth²) in
// output size, so a ~10k-deep chain in a 200KB session blob expands to a ~700MB
// root blob that then gets written and pushed.
const MaxSubagentDepth = 16

// AddTokenUsage returns the sum of a and b, recursing into subagent usage.
// Either operand may be nil (treated as zero); the result is nil only when both
// are. Neither input is mutated. Subagent chains deeper than MaxSubagentDepth are
// truncated.
func AddTokenUsage(a, b *TokenUsage) *TokenUsage {
	return addTokenUsageAtDepth(a, b, 0)
}

func addTokenUsageAtDepth(a, b *TokenUsage, depth int) *TokenUsage {
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
	if depth >= MaxSubagentDepth {
		return sum
	}
	sum.SubagentTokens = addTokenUsageAtDepth(aSub, bSub, depth+1)
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
