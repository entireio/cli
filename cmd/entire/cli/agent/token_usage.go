package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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
	fallbackModel = resolveTierFallback(fallbackModel, table)

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

	applyTierVariant(buckets, fallbackModel)

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

	// Revert unpriceable variant ids (e.g. a fast turn on a model with no -fast
	// rate) to their base so they estimate at the base rate instead of going
	// unpriced under a premium-claiming id the table can't resolve.
	downgradeUnpriceableVariants(buckets, table)

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

// EstimateCost computes a LOCAL, on-the-fly USD cost estimate for an
// already-recorded token breakdown, for DISPLAY ONLY. The CLI no longer persists
// cost — entire-api prices server-side from the token breakdown — so the token
// commands (`entire session tokens`, `entire checkpoint tokens`, `entire tokens
// profile`) call this to show a clearly-labeled local estimate alongside the
// token counts.
//
// It prefers the persisted per-model buckets, pricing each priceable bucket at
// table's current rates and folding them exactly as CalculateUsageWithCost's
// pricing pass does. When no buckets are supplied it falls back to pricing the
// flat usage (subagent tokens flattened in) under fallbackModel. It never trusts
// any pre-existing cost — every bucket is re-estimated — so the result is a pure
// current-rate estimate whose CostSource is CostSourceEstimated (or
// CostSourceMixed when some tokens are priceable and some are not). Returns
// (nil, "") when nothing is priceable (nil table, unknown models, or no billable
// tokens) so callers render no cost, never $0.
func EstimateCost(usage *types.TokenUsage, models []types.ModelUsage, fallbackModel string, table *pricing.Table) (*float64, string) {
	if table == nil {
		return nil, ""
	}
	var buckets []types.ModelUsage
	switch {
	case len(models) > 0:
		buckets = make([]types.ModelUsage, len(models))
		for i := range models {
			buckets[i] = types.ModelUsage{Model: models[i].Model, TokenUsage: bucketTokens(&models[i].TokenUsage)}
		}
	case usage != nil:
		// No per-model breakdown: price the flat total, flattening the subagent
		// subtree into the single bucket so subagent tokens are not dropped.
		buckets = []types.ModelUsage{{Model: fallbackModel, TokenUsage: flattenTokenUsage(usage)}}
	default:
		return nil, ""
	}
	priceBuckets(buckets, table, false)
	return foldBucketCost(buckets)
}

// tierVariantSuffixes are the pricing-tier decorations a caller may have
// appended to fallbackModel via settings.PricingModelForAgent (e.g. the Codex
// priority service tier). They exist in the pricing table as distinct model
// ids but never appear in transcripts. This is the subset the fallback-model
// machinery (resolveTierFallback/applyTierVariant) is responsible for.
var tierVariantSuffixes = []string{"-priority"}

// variantSuffixes lists every pricing decoration a bucket's model id may carry
// that the table might not price: "-fast" (appended by claudecode's
// modelKeyWithSpeed for fast-mode turns) and "-priority" (the Codex service-tier
// knob). downgradeUnpriceableVariants reverts an unpriceable one to its base id.
// It is the superset of tierVariantSuffixes because a speed variant reaches a
// bucket through a ModelUsageCalculator, not through the fallback model.
var variantSuffixes = []string{"-fast", "-priority"}

// resolveTierFallback downgrades a tier-suffixed fallback model to its base id
// when the table cannot price the variant. The tier knob suffixes whatever
// model the agent reports, but only some variants have published rates (e.g.
// gpt-5.5-priority): pricing a gpt-5.3-codex session under a nonexistent
// gpt-5.3-codex-priority id would leave it entirely unpriced — worse than the
// standard-rate estimate, and the suffixed bucket id would claim a premium that
// was never applied. Falling back to the base id keeps the estimate an honest
// undercount, mirroring how fast turns on models without a published fast rate
// price at the base id. With a nil table nothing can be priced or verified, so
// the base id is kept there too.
func resolveTierFallback(fallbackModel string, table *pricing.Table) string {
	for _, suffix := range tierVariantSuffixes {
		base := strings.TrimSuffix(fallbackModel, suffix)
		if base == fallbackModel || base == "" {
			continue
		}
		if table != nil {
			if _, ok := table.Lookup(fallbackModel); ok {
				return fallbackModel
			}
		}
		return base
	}
	return fallbackModel
}

// applyTierVariant retargets per-model buckets onto the caller's tier-variant
// pricing id. When fallbackModel carries a known tier suffix (the caller opted
// the session into that service tier), a ModelUsageCalculator's buckets still
// carry the raw transcript model id — priced as-is they'd silently fall back to
// standard rates, re-introducing the tier-loss defect the suffixed fallback
// fixed on the no-calculator path. Only buckets matching the fallback's base id
// are remapped: other models have no table entry for the variant, and an
// unpriceable id would be worse than a standard-rate estimate.
func applyTierVariant(buckets []types.ModelUsage, fallbackModel string) {
	for _, suffix := range tierVariantSuffixes {
		base := strings.TrimSuffix(fallbackModel, suffix)
		if base == fallbackModel || base == "" {
			continue
		}
		for i := range buckets {
			if buckets[i].Model == base {
				buckets[i].Model = fallbackModel
			}
		}
	}
}

// downgradeUnpriceableVariants reverts, in place, each bucket whose model id
// carries a known variant suffix (-fast, -priority) to its base id when the table
// cannot price the variant but can price the base. A fast turn on a model with no
// published fast rate (e.g. claude-fable-5-fast) would otherwise miss the table
// entirely and go unpriced — worse than the honest base-rate estimate, under an
// id claiming a premium that was never applied. Detection stays truthful at the
// source (modelKeyWithSpeed, the tier knob); only here, at pricing time, is
// priceability known. This is the calculator-bucket analogue of
// resolveTierFallback, which only covers the fallback model.
//
// Rekeying is in place, not merging: a downgraded bucket may now share a model id
// with another bucket, but same-model buckets are already emitted today (a
// ModelUsageCalculator's per-model bucket plus the remainder bucket both sit under
// the session model), and both foldBucketCost here and strategy.accumulateModelUsage
// downstream fold buckets by model key. Leaving the duplicate is therefore the
// token- and cost-conserving choice, and it keeps the pre-existing remainder
// behavior (separate same-model buckets) unchanged.
func downgradeUnpriceableVariants(buckets []types.ModelUsage, table *pricing.Table) {
	for i := range buckets {
		if base, ok := downgradedVariantBase(buckets[i].Model, table); ok {
			buckets[i].Model = base
		}
	}
}

// downgradedVariantBase returns the base id a variant-suffixed model should be
// priced under, and whether the downgrade applies. It reports (base, true) when
// the model carries a known variant suffix and either the table is nil (nothing
// can be verified, so fall back — mirroring resolveTierFallback), or the table
// cannot price the variant but can price the base. It reports ("", false) for a
// priceable variant (keep it), a variant whose base is also unpriceable (the base
// is no better and the variant id is the more truthful label), or a model with no
// known variant suffix.
func downgradedVariantBase(model string, table *pricing.Table) (string, bool) {
	for _, suffix := range variantSuffixes {
		base := strings.TrimSuffix(model, suffix)
		if base == model || base == "" {
			continue
		}
		if table == nil {
			return base, true
		}
		if _, ok := table.Lookup(model); ok {
			return "", false
		}
		if _, ok := table.Lookup(base); ok {
			return base, true
		}
		return "", false
	}
	return "", false
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
	if short.InputTokens+short.CacheCreationTokens+short.CacheReadTokens+short.OutputTokens == 0 {
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
//
// Defensive normalization: a bucket carrying a non-nil CostUSD with an empty
// CostSource (currently unreachable — priceBuckets always pairs a cost it sets
// with CostSourceEstimated, and every other producer of a priced bucket is
// expected to set a source too) is treated as CostSourceEstimated here rather
// than folding into a costed-but-unlabeled total that would render with no
// reported/estimated/mixed suffix.
func foldBucketCost(buckets []types.ModelUsage) (*float64, string) {
	var cost *float64
	var source string
	anyPriced := false
	anyUnpricedTokens := false
	for i := range buckets {
		b := buckets[i].TokenUsage
		bSource := b.CostSource
		if b.CostUSD != nil && bSource == "" {
			bSource = types.CostSourceEstimated
		}
		source = types.MergeCostSource(source, bSource, cost, b.CostUSD)
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
