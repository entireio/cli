package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
)

// CalculateTokenUsage calculates token usage from transcript data.
// Returns nil if the agent doesn't support token calculation or on error.
// Errors are debug-logged because callers treat nil token usage as "no data available".
func CalculateTokenUsage(ctx context.Context, ag Agent, transcriptData []byte, transcriptLinesAtStart int, subagentsDir string) *TokenUsage {
	usage, err := flatTokenUsage(ag, transcriptData, transcriptLinesAtStart, subagentsDir)
	if err != nil {
		logging.Debug(ctx, "failed token extraction",
			slog.String("error", err.Error()))
		return nil
	}
	return usage
}

// flatTokenUsage computes the flat (model-agnostic) token usage using the same
// dispatch preference as CalculateTokenUsage: SubagentAwareExtractor first (to
// include subagent tokens), then TokenCalculator, else a nil usage. It returns
// the agent's error instead of logging so cost-aware callers can decide; the
// wrapped prefix preserves the previous per-path distinction in logs.
func flatTokenUsage(ag Agent, transcriptData []byte, fromOffset int, subagentsDir string) (*TokenUsage, error) {
	if ag == nil {
		return nil, nil //nolint:nilnil // (nil, nil) means "no token data available", which is not an error
	}
	if subagentExtractor, ok := AsSubagentAwareExtractor(ag); ok {
		usage, err := subagentExtractor.CalculateTotalTokenUsage(transcriptData, fromOffset, subagentsDir)
		if err != nil {
			return nil, fmt.Errorf("subagent-aware token extraction: %w", err)
		}
		return usage, nil
	}
	if calculator, ok := AsTokenCalculator(ag); ok {
		usage, err := calculator.CalculateTokenUsage(transcriptData, fromOffset)
		if err != nil {
			return nil, fmt.Errorf("token extraction: %w", err)
		}
		return usage, nil
	}
	return nil, nil //nolint:nilnil // (nil, nil) means "no token data available", which is not an error
}

// CalculateUsageWithCost computes the flat TokenUsage exactly as CalculateTokenUsage
// does, then attributes cost. Per-model buckets come from a ModelUsageCalculator
// when the agent implements one; otherwise the flat usage becomes a single bucket
// under fallbackModel (an empty fallbackModel yields an unpriceable bucket). Each
// unpriced bucket is estimated via table when estimation is enabled and the
// model resolves; a nil table or a lookup miss leaves the bucket's cost nil. The
// flat cost is the sum of bucket costs and its source folds the bucket sources
// (CostSourceMixed when priced and unpriced-with-tokens buckets coexist).
// Returns the flat usage (cost fields populated) and the buckets; (nil, nil, nil)
// when the agent produces no usage.
func CalculateUsageWithCost(ag Agent, transcriptData []byte, fromOffset int, subagentsDir string, table *pricing.Table, fallbackModel string, disableEstimation bool) (*types.TokenUsage, []types.ModelUsage, error) {
	flat, err := flatTokenUsage(ag, transcriptData, fromOffset, subagentsDir)
	if err != nil {
		return nil, nil, err
	}
	if flat == nil {
		return nil, nil, nil
	}

	buckets, err := modelUsageBuckets(ag, transcriptData, fromOffset, flat, fallbackModel)
	if err != nil {
		return nil, nil, err
	}

	// The per-model buckets can cover fewer tokens than the flat total: an
	// agent's CalculateModelUsage sees only the main transcript, while the flat
	// path (SubagentAwareExtractor) may fold in extra usage the buckets never
	// saw. Left alone, that shortfall would go unpriced, silently undercounting
	// cost while CostSource still read "estimated". Attribute any shortfall to a
	// remainder bucket under fallbackModel so the pricing pass either estimates
	// it (priceable fallback) or leaves it unpriced (unpriceable fallback), in
	// which case foldBucketCost's mixed rule marks coverage honestly.
	if rem, ok := remainderBucket(flat, buckets, fallbackModel); ok {
		buckets = append(buckets, rem)
	}

	priceBuckets(buckets, table, disableEstimation)
	cost, source := foldBucketCost(buckets)
	flat.CostUSD = cost
	flat.CostSource = source
	return flat, buckets, nil
}

// PriceUsage attributes cost to an already-computed flat TokenUsage (e.g. one an
// agent reported through its stop hook, like Cursor) by treating it as a single
// bucket under model and running the same pricing pass as CalculateUsageWithCost.
// A usage that already carries a reported cost is preserved. Returns a copy of
// usage with its cost fields populated plus the single bucket; (nil, nil) when
// usage is nil.
func PriceUsage(usage *types.TokenUsage, model string, table *pricing.Table, disableEstimation bool) (*types.TokenUsage, []types.ModelUsage) {
	if usage == nil {
		return nil, nil
	}
	// Build the single bucket without aliasing usage.SubagentTokens: a shared
	// subtree pointer would let a downstream in-place aggregator mutate the
	// caller's usage. Estimate prices only the scalar fields, so dropping the
	// subtree does not change the computed cost. A reported cost is preserved so
	// priceBuckets leaves it untouched.
	bucket := *usage
	bucket.SubagentTokens = nil
	buckets := []types.ModelUsage{{Model: model, TokenUsage: bucket}}
	priceBuckets(buckets, table, disableEstimation)
	cost, source := foldBucketCost(buckets)
	out := *usage
	out.CostUSD = cost
	out.CostSource = source
	return &out, buckets
}

// modelUsageBuckets returns per-model token buckets for the transcript slice. It
// prefers a ModelUsageCalculator; otherwise the whole flat usage becomes a
// single bucket under fallbackModel with its cost fields cleared (buckets carry
// token counts only).
func modelUsageBuckets(ag Agent, transcriptData []byte, fromOffset int, flat *types.TokenUsage, fallbackModel string) ([]types.ModelUsage, error) {
	if calc, ok := AsModelUsageCalculator(ag); ok {
		buckets, err := calc.CalculateModelUsage(transcriptData, fromOffset)
		if err != nil {
			return nil, fmt.Errorf("model usage attribution: %w", err)
		}
		return buckets, nil
	}
	return []types.ModelUsage{{Model: fallbackModel, TokenUsage: bucketTokens(flat)}}, nil
}

// remainderBucket returns a bucket carrying the per-field token shortfall
// between the flat total (INCLUDING every nested SubagentTokens subtree) and the
// sum of the per-model buckets, attributed to fallbackModel. The flat side is
// flattened via flattenTokenUsage because the per-model buckets (and, per
// bucketTokens, the fallback single bucket) carry only main-scoped scalar
// counts, while a SubagentAwareExtractor folds subagent usage into the flat
// total's SubagentTokens subtree. Without accounting for that subtree the
// subagent tokens would go entirely unpriced — e.g. a Claude Code session with
// 100k main + 500k subagent tokens would price only the 100k. Here the 500k
// shortfall becomes a remainder bucket under fallbackModel.
//
// Each field is clamped at 0 (a bucket may legitimately exceed the flat total on
// an individual field). The bool is false when no billable token shortfall
// exists: the fallback (single-bucket) path with no subagents and any
// ModelUsageCalculator whose buckets sum to the flat total both yield a zero
// remainder. Cost fields are left nil so the pricing pass estimates the
// remainder when fallbackModel is priceable, or leaves it unpriced otherwise.
func remainderBucket(flat *types.TokenUsage, buckets []types.ModelUsage, fallbackModel string) (types.ModelUsage, bool) {
	var sum types.TokenUsage
	for i := range buckets {
		b := buckets[i].TokenUsage
		sum.InputTokens += b.InputTokens
		sum.CacheCreationTokens += b.CacheCreationTokens
		sum.CacheReadTokens += b.CacheReadTokens
		sum.OutputTokens += b.OutputTokens
		sum.APICallCount += b.APICallCount
	}
	flatTotal := flattenTokenUsage(flat)
	short := types.TokenUsage{
		InputTokens:         clampNonNegative(flatTotal.InputTokens - sum.InputTokens),
		CacheCreationTokens: clampNonNegative(flatTotal.CacheCreationTokens - sum.CacheCreationTokens),
		CacheReadTokens:     clampNonNegative(flatTotal.CacheReadTokens - sum.CacheReadTokens),
		OutputTokens:        clampNonNegative(flatTotal.OutputTokens - sum.OutputTokens),
		APICallCount:        clampNonNegative(flatTotal.APICallCount - sum.APICallCount),
	}
	if short.InputTokens+short.CacheCreationTokens+short.CacheReadTokens+short.OutputTokens+short.APICallCount == 0 {
		return types.ModelUsage{}, false
	}
	return types.ModelUsage{Model: fallbackModel, TokenUsage: short}, true
}

// flattenTokenUsage returns u's five scalar token fields summed with every
// nested SubagentTokens subtree, at arbitrary depth. It is nil-safe (nil yields
// a zero usage) and reads only token counts — the returned usage carries no cost
// fields. This is how remainderBucket recovers the true billable total from a
// flat usage whose subagent tokens live in a subtree rather than in the top-level
// scalar fields.
func flattenTokenUsage(u *types.TokenUsage) types.TokenUsage {
	var out types.TokenUsage
	if u == nil {
		return out
	}
	out.InputTokens = u.InputTokens
	out.CacheCreationTokens = u.CacheCreationTokens
	out.CacheReadTokens = u.CacheReadTokens
	out.OutputTokens = u.OutputTokens
	out.APICallCount = u.APICallCount
	sub := flattenTokenUsage(u.SubagentTokens)
	out.InputTokens += sub.InputTokens
	out.CacheCreationTokens += sub.CacheCreationTokens
	out.CacheReadTokens += sub.CacheReadTokens
	out.OutputTokens += sub.OutputTokens
	out.APICallCount += sub.APICallCount
	return out
}

// clampNonNegative returns v, or 0 when v is negative.
func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// bucketTokens returns a copy of u with its cost fields cleared and its
// SubagentTokens subtree dropped, so it serves as a per-model bucket carrying
// main-scoped scalar token counts only. Nil-ing SubagentTokens is essential:
// remainderBucket attributes the subagent subtree to its own remainder bucket,
// so a fallback bucket that also carried the shared subtree pointer would both
// double-count the subagent tokens downstream and alias u's subtree (a mutation
// hazard once aggregators fold buckets in place).
func bucketTokens(u *types.TokenUsage) types.TokenUsage {
	b := *u
	b.CostUSD = nil
	b.CostSource = ""
	b.SubagentTokens = nil
	return b
}

// priceBuckets fills in each bucket's cost in place: a bucket that already has a
// reported cost is left untouched; otherwise, when estimation is enabled and the
// table resolves the bucket's model, the cost is estimated (CostSourceEstimated).
// A bucket whose model is unknown, a bucket carrying no billable tokens, or any
// bucket when the table is nil or estimation is disabled, keeps a nil cost.
func priceBuckets(buckets []types.ModelUsage, table *pricing.Table, disableEstimation bool) {
	for i := range buckets {
		b := &buckets[i].TokenUsage
		if b.CostUSD != nil {
			continue // reported cost wins (even on a zero-token bucket)
		}
		if disableEstimation || table == nil {
			continue
		}
		if !bucketHasTokens(*b) {
			// No billable tokens: leave the cost nil rather than fabricating a
			// $0.00 "estimated" figure. Mirrors the ModelUsageCalculator path and
			// keeps zero-usage checkpoints at "no cost data" instead of flipping
			// aggregated provenance from reported to mixed.
			continue
		}
		rate, ok := table.Lookup(buckets[i].Model)
		if !ok {
			continue
		}
		cost := pricing.Estimate(rate, *b)
		b.CostUSD = &cost
		b.CostSource = types.CostSourceEstimated
	}
}

// foldBucketCost aggregates per-bucket costs into a single (cost, source) pair.
// Costs sum via AddCostUSD and sources fold via MergeCostSource; when at least
// one bucket was priced and at least one bucket has tokens but no cost, the
// source is CostSourceMixed to signal partial coverage.
func foldBucketCost(buckets []types.ModelUsage) (*float64, string) {
	var cost *float64
	var source string
	anyPriced := false
	anyUnpricedTokens := false
	for i := range buckets {
		b := buckets[i].TokenUsage
		source = types.MergeCostSource(source, b.CostSource, cost, b.CostUSD)
		cost = types.AddCostUSD(cost, b.CostUSD)
		switch {
		case b.CostUSD != nil:
			anyPriced = true
		case bucketHasTokens(b):
			anyUnpricedTokens = true
		}
	}
	if anyPriced && anyUnpricedTokens {
		return cost, types.CostSourceMixed
	}
	return cost, source
}

// bucketHasTokens reports whether a bucket carries any billable token count.
func bucketHasTokens(u types.TokenUsage) bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheCreationTokens > 0
}
