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

// MergeCostSourceUsages merges the cost-source provenance of two usages. It
// behaves like MergeCostSource over the two sides' (CostSource, CostUSD) pairs
// but additionally treats a token-bearing side with no cost as a distinct
// provenance: when exactly one side carries a non-nil cost and the other side
// has any nonzero token field with a nil cost, the merge is CostSourceMixed.
// This mirrors foldBucketCost's partial-coverage rule, so a priced usage merged
// with an unpriced-but-token-bearing usage honestly reports mixed coverage
// instead of masquerading as fully priced. Either usage may be nil (contributes
// nothing). APICallCount and SubagentTokens are not treated as billable tokens.
func MergeCostSourceUsages(a, b *TokenUsage) string {
	var aSource, bSource string
	var aCost, bCost *float64
	if a != nil {
		aSource, aCost = a.CostSource, a.CostUSD
	}
	if b != nil {
		bSource, bCost = b.CostSource, b.CostUSD
	}

	// Exactly one side priced while the other carries unpriced tokens -> mixed.
	if aCost != nil && bCost == nil && usageHasBillableTokens(b) {
		return CostSourceMixed
	}
	if bCost != nil && aCost == nil && usageHasBillableTokens(a) {
		return CostSourceMixed
	}
	return MergeCostSource(aSource, bSource, aCost, bCost)
}

// usageHasBillableTokens reports whether u carries any nonzero billable token
// count (input, cache-creation, cache-read, or output). It is nil-safe.
func usageHasBillableTokens(u *TokenUsage) bool {
	return u != nil && (u.InputTokens != 0 || u.CacheCreationTokens != 0 || u.CacheReadTokens != 0 || u.OutputTokens != 0)
}
