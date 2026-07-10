package agent

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDowngradedVariantBase covers the priceability rule that decides whether a
// variant-suffixed model id is reverted to its base for pricing: downgrade only
// when the base is a strictly better (priceable) target than the unpriceable
// variant, or when the table is nil.
func TestDowngradedVariantBase(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	require.NoError(t, err)

	tests := []struct {
		name     string
		model    string
		table    *pricing.Table
		wantBase string
		wantOK   bool
	}{
		{"unpriceable fast variant downgrades to base", "claude-fable-5-fast", table, "claude-fable-5", true},
		{"priceable fast variant is kept", "claude-opus-4-8-fast", table, "", false},
		{"unpriceable priority variant downgrades to base", "gpt-5.3-codex-priority", table, "gpt-5.3-codex", true},
		{"priceable priority variant is kept", "gpt-5.5-priority", table, "", false},
		{"no variant suffix is a no-op", "claude-fable-5", table, "", false},
		{"variant with unpriceable base is kept truthful", "totally-unknown-model-fast", table, "", false},
		{"bare suffix has no base", "-fast", table, "", false},
		{"empty model is a no-op", "", table, "", false},
		{"nil table downgrades the variant", "claude-fable-5-fast", nil, "claude-fable-5", true},
		{"nil table downgrades a priority variant too", "gpt-5.5-priority", nil, "gpt-5.5", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base, ok := downgradedVariantBase(tt.model, tt.table)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantBase, base)
		})
	}
}

// TestDowngradeUnpriceableVariants covers the in-place bucket rekeying: only
// unpriceable-variant buckets move to their base; priceable variants and plain
// ids stay put.
func TestDowngradeUnpriceableVariants(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	require.NoError(t, err)

	models := func(buckets []types.ModelUsage) []string {
		out := make([]string, 0, len(buckets))
		for _, b := range buckets {
			out = append(out, b.Model)
		}
		return out
	}
	mk := func(ids ...string) []types.ModelUsage {
		buckets := make([]types.ModelUsage, 0, len(ids))
		for _, id := range ids {
			buckets = append(buckets, types.ModelUsage{Model: id})
		}
		return buckets
	}

	t.Run("only the unpriceable fast bucket moves", func(t *testing.T) {
		t.Parallel()
		buckets := mk("claude-opus-4-8-fast", "claude-fable-5-fast", "claude-fable-5", "")
		downgradeUnpriceableVariants(buckets, table)
		assert.Equal(t, []string{"claude-opus-4-8-fast", "claude-fable-5", "claude-fable-5", ""}, models(buckets))
	})

	t.Run("nil table downgrades every variant", func(t *testing.T) {
		t.Parallel()
		buckets := mk("claude-fable-5-fast", "gpt-5.5-priority", "claude-opus-4-8")
		downgradeUnpriceableVariants(buckets, nil)
		assert.Equal(t, []string{"claude-fable-5", "gpt-5.5", "claude-opus-4-8"}, models(buckets))
	})
}

// TestCalculateUsageWithCost_UnpriceableFastDowngradesToBase is the regression
// for the finding: a fast-mode turn on a model with no published -fast rate must
// estimate at the base rate under the base id, not go unpriced (nil) under the
// unpriceable -fast id. claude-fable-5 has no -fast entry; its base bills $10/$50.
func TestCalculateUsageWithCost_UnpriceableFastDowngradesToBase(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	require.NoError(t, err)

	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		buckets: []types.ModelUsage{
			{Model: "claude-fable-5-fast", TokenUsage: TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", table, "", false)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	// Rekeyed to the base id and priced at the base rate, not left unpriced.
	assert.Equal(t, "claude-fable-5", buckets[0].Model)
	require.NotNil(t, buckets[0].TokenUsage.CostUSD)
	assert.InDelta(t, 60.0, *buckets[0].TokenUsage.CostUSD, 1e-9) // 1M@$10 + 1M@$50
	assert.Equal(t, types.CostSourceEstimated, buckets[0].TokenUsage.CostSource)
	require.NotNil(t, flat.CostUSD)
	assert.InDelta(t, 60.0, *flat.CostUSD, 1e-9)
	assert.Equal(t, types.CostSourceEstimated, flat.CostSource)
}

// TestCalculateUsageWithCost_MixedFastPriceableAndUnpriceable proves a session
// mixing a priceable fast model with an unpriceable one handles each correctly
// and conserves the total: opus-4-8-fast keeps its premium, fable-5-fast falls
// back to the fable base.
func TestCalculateUsageWithCost_MixedFastPriceableAndUnpriceable(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	require.NoError(t, err)

	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000, OutputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "claude-opus-4-8-fast", TokenUsage: TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
			{Model: "claude-fable-5-fast", TokenUsage: TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", table, "", false)
	require.NoError(t, err)
	require.Len(t, buckets, 2)

	// Priceable fast variant keeps its premium id and rate: 1M@$10 + 1M@$50 = 60.
	assert.Equal(t, "claude-opus-4-8-fast", buckets[0].Model)
	require.NotNil(t, buckets[0].TokenUsage.CostUSD)
	assert.InDelta(t, 60.0, *buckets[0].TokenUsage.CostUSD, 1e-9)

	// Unpriceable fast variant downgrades to the base id and rate: also $60 here.
	assert.Equal(t, "claude-fable-5", buckets[1].Model)
	require.NotNil(t, buckets[1].TokenUsage.CostUSD)
	assert.InDelta(t, 60.0, *buckets[1].TokenUsage.CostUSD, 1e-9)

	// Conservation: flat cost is the sum, all estimated.
	require.NotNil(t, flat.CostUSD)
	assert.InDelta(t, 120.0, *flat.CostUSD, 1e-9)
	assert.Equal(t, types.CostSourceEstimated, flat.CostSource)
}

// TestCalculateUsageWithCost_DowngradedFastCollidesWithRemainderConserved proves
// the leave-duplicate choice: when a downgraded fast bucket ends up under the same
// base id as the remainder bucket, both are kept (as same-model buckets already
// are today) and the total is conserved — foldBucketCost sums them and downstream
// folds by model key.
func TestCalculateUsageWithCost_DowngradedFastCollidesWithRemainderConserved(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	require.NoError(t, err)

	// flat exceeds the single calculator bucket, so a remainder bucket is emitted
	// under fallbackModel "claude-fable-5"; the calculator's fast bucket downgrades
	// onto that same base id.
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000, OutputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "claude-fable-5-fast", TokenUsage: TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", table, "claude-fable-5", false)
	require.NoError(t, err)
	require.Len(t, buckets, 2)

	var totalIn, totalOut int
	for _, b := range buckets {
		assert.Equal(t, "claude-fable-5", b.Model, "both buckets under the base id after downgrade")
		require.NotNil(t, b.TokenUsage.CostUSD)
		assert.InDelta(t, 60.0, *b.TokenUsage.CostUSD, 1e-9)
		totalIn += b.TokenUsage.InputTokens
		totalOut += b.TokenUsage.OutputTokens
	}
	assert.Equal(t, 2_000_000, totalIn, "input tokens conserved across the duplicate buckets")
	assert.Equal(t, 2_000_000, totalOut, "output tokens conserved across the duplicate buckets")

	require.NotNil(t, flat.CostUSD)
	assert.InDelta(t, 120.0, *flat.CostUSD, 1e-9)
	assert.Equal(t, types.CostSourceEstimated, flat.CostSource)
}
