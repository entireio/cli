package types

// Cost source provenance values for TokenUsage.CostSource.
const (
	// CostSourceReported means the cost came directly from an agent-reported
	// figure (authoritative).
	CostSourceReported = "reported"
	// CostSourceEstimated means the cost was derived from a pricing estimate.
	CostSourceEstimated = "estimated"
	// CostSourceMixed means the aggregated cost combined differing provenances
	// (e.g. some reported, some estimated).
	CostSourceMixed = "mixed"
)

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
	// CostUSD is the total cost in USD for this usage, when known.
	// nil means unknown (no agent-reported cost and no estimate) — never treat as $0.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// CostSource records provenance: CostSourceReported, CostSourceEstimated, or CostSourceMixed.
	CostSource string `json:"cost_source,omitempty"`
}

// ModelUsage pairs a model identifier with its token usage. It is used by
// per-model cost accounting (later chunks); harmless to carry now.
type ModelUsage struct {
	Model      string     `json:"model"`
	TokenUsage TokenUsage `json:"token_usage"`
}

// AddCostUSD sums two optional cost values, returning a NEW pointer that never
// aliases either input.
//
//   - nil + nil -> nil
//   - nil + x   -> copy of x
//   - x + y     -> x + y
func AddCostUSD(a, b *float64) *float64 {
	if a == nil && b == nil {
		return nil
	}
	var sum float64
	if a != nil {
		sum += *a
	}
	if b != nil {
		sum += *b
	}
	return &sum
}

// MergeCostSource merges two cost-source provenance labels. A side whose cost
// is nil contributes no source (its label is ignored, since it carries no
// cost). Both effective sides empty -> ""; equal non-empty -> that value;
// differing non-empty -> CostSourceMixed.
func MergeCostSource(a, b string, aCost, bCost *float64) string {
	if aCost == nil {
		a = ""
	}
	if bCost == nil {
		b = ""
	}
	switch {
	case a == "" && b == "":
		return ""
	case a == "":
		return b
	case b == "":
		return a
	case a == b:
		return a
	default:
		return CostSourceMixed
	}
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
