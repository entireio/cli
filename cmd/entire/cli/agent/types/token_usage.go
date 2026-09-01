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

	// ThinkingTokens is the part of OutputTokens the model spent on reasoning
	// ("thinking"/"reasoning"/"thoughts"). It is a SUBSET of OutputTokens, not a
	// fifth class: the four classes above still sum to the provider's total.
	// Read verbatim from the agent's own usage fields (Claude Code
	// output_tokens_details.thinking_tokens, Codex reasoning_output_tokens,
	// OpenCode tokens.reasoning, Gemini tokens.thoughts); 0 when the agent does
	// not record it (Pi, Cursor, pre-Aug-2026 Claude transcripts) — readers must
	// treat "absent" as "not recorded", never as zero thinking.
	ThinkingTokens int `json:"thinking_tokens,omitempty"`
	// CacheCreation1hTokens is the part of CacheCreationTokens written with a
	// 1-hour TTL (priced 2× input by Anthropic, vs 1.25× for the 5-minute TTL).
	// SUBSET of CacheCreationTokens. Anthropic-only; 0 elsewhere.
	CacheCreation1hTokens int `json:"cache_creation_1h_tokens,omitempty"`
	// Model that produced these tokens when it differs from the owning session's
	// model — set on subagent entries so cost can be weighted per model. Empty on
	// a top-level entry (the session metadata's `model` applies) and cleared when
	// entries on different models are summed.
	Model string `json:"model,omitempty"`

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
		sum.ThinkingTokens = a.ThinkingTokens
		sum.CacheCreation1hTokens = a.CacheCreation1hTokens
		sum.Model = a.Model
		aSub = a.SubagentTokens
	}
	if b != nil {
		sum.InputTokens = clampAdd(sum.InputTokens, b.InputTokens)
		sum.CacheCreationTokens = clampAdd(sum.CacheCreationTokens, b.CacheCreationTokens)
		sum.CacheReadTokens = clampAdd(sum.CacheReadTokens, b.CacheReadTokens)
		sum.OutputTokens = clampAdd(sum.OutputTokens, b.OutputTokens)
		sum.APICallCount = clampAdd(sum.APICallCount, b.APICallCount)
		sum.ThinkingTokens = clampAdd(sum.ThinkingTokens, b.ThinkingTokens)
		sum.CacheCreation1hTokens = clampAdd(sum.CacheCreation1hTokens, b.CacheCreation1hTokens)
		sum.Model = mergeModel(sum.Model, b.Model)
		bSub = b.SubagentTokens
	}
	if depth >= MaxSubagentDepth {
		return sum
	}
	sum.SubagentTokens = addTokenUsageAtDepth(aSub, bSub, depth+1)
	return sum
}

// clampAdd returns a+b, saturating at the maximum int rather than wrapping.
// Token counts never approach it in practice, but a wrapped total would render
// as a negative or absurd figure in a user-facing report, so overflow degrades
// to "impossibly large" instead.
func clampAdd(a, b int) int {
	const maxInt = int(^uint(0) >> 1)
	if a > 0 && b > maxInt-a {
		return maxInt
	}
	return a + b
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
		InputTokens:           clampSubtract(a.InputTokens, b.InputTokens),
		CacheCreationTokens:   clampSubtract(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheReadTokens:       clampSubtract(a.CacheReadTokens, b.CacheReadTokens),
		OutputTokens:          clampSubtract(a.OutputTokens, b.OutputTokens),
		APICallCount:          clampSubtract(a.APICallCount, b.APICallCount),
		ThinkingTokens:        clampSubtract(a.ThinkingTokens, b.ThinkingTokens),
		CacheCreation1hTokens: clampSubtract(a.CacheCreation1hTokens, b.CacheCreation1hTokens),
		Model:                 a.Model,
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

// mergeModel combines the Model labels of two summed entries: equal or one-sided
// labels are kept, two different labels become "" (mixed), so a per-model cost
// weight is never applied to tokens from another model.
func mergeModel(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "" || a == b:
		return a
	default:
		return ""
	}
}
