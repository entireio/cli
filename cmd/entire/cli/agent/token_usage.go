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
	buckets := []types.ModelUsage{{Model: model, TokenUsage: *usage}}
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
// between the flat total and the sum of the per-model buckets, attributed to
// fallbackModel. Each field is clamped at 0 (a bucket may legitimately exceed
// the flat total on an individual field). The bool is false when no billable
// token shortfall exists, which is the common case: the fallback (single-bucket)
// path and any ModelUsageCalculator whose buckets sum to the flat total both
// yield a zero remainder. Cost fields are left nil so the pricing pass estimates
// the remainder when fallbackModel is priceable, or leaves it unpriced otherwise.
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
	short := types.TokenUsage{
		InputTokens:         clampNonNegative(flat.InputTokens - sum.InputTokens),
		CacheCreationTokens: clampNonNegative(flat.CacheCreationTokens - sum.CacheCreationTokens),
		CacheReadTokens:     clampNonNegative(flat.CacheReadTokens - sum.CacheReadTokens),
		OutputTokens:        clampNonNegative(flat.OutputTokens - sum.OutputTokens),
		APICallCount:        clampNonNegative(flat.APICallCount - sum.APICallCount),
	}
	if short.InputTokens+short.CacheCreationTokens+short.CacheReadTokens+short.OutputTokens+short.APICallCount == 0 {
		return types.ModelUsage{}, false
	}
	return types.ModelUsage{Model: fallbackModel, TokenUsage: short}, true
}

// clampNonNegative returns v, or 0 when v is negative.
func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// bucketTokens returns a copy of u with its cost fields cleared, so it can serve
// as a per-model bucket that carries token counts only.
func bucketTokens(u *types.TokenUsage) types.TokenUsage {
	b := *u
	b.CostUSD = nil
	b.CostSource = ""
	return b
}

// priceBuckets fills in each bucket's cost in place: a bucket that already has a
// reported cost is left untouched; otherwise, when estimation is enabled and the
// table resolves the bucket's model, the cost is estimated (CostSourceEstimated).
// A bucket whose model is unknown, or any bucket when the table is nil or
// estimation is disabled, keeps a nil cost.
func priceBuckets(buckets []types.ModelUsage, table *pricing.Table, disableEstimation bool) {
	for i := range buckets {
		b := &buckets[i].TokenUsage
		if b.CostUSD != nil {
			continue // reported cost wins
		}
		if disableEstimation || table == nil {
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
