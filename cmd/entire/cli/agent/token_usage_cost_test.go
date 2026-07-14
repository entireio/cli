package agent

import (
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
)

// --- Fakes ---

// fakeTokenCalcAgent implements TokenCalculator only (no per-model attribution).
type fakeTokenCalcAgent struct {
	mockBaseAgent

	usage *TokenUsage
	err   error
}

func (f *fakeTokenCalcAgent) CalculateTokenUsage([]byte, int) (*TokenUsage, error) {
	return f.usage, f.err
}

// fakeModelUsageAgent implements both TokenCalculator (for the flat total) and
// ModelUsageCalculator (for per-model buckets).
type fakeModelUsageAgent struct {
	mockBaseAgent

	flat    *TokenUsage
	buckets []types.ModelUsage
	muErr   error
}

func (f *fakeModelUsageAgent) CalculateTokenUsage([]byte, int) (*TokenUsage, error) { //nolint:unparam // test mock: error result satisfies TokenCalculator
	return f.flat, nil
}

func (f *fakeModelUsageAgent) CalculateModelUsage([]byte, int) ([]types.ModelUsage, error) {
	return f.buckets, f.muErr
}

// fakeSubagentAwareAgent implements SubagentAwareExtractor (its flat total folds
// subagent usage into a SubagentTokens subtree) but NOT ModelUsageCalculator, so
// CalculateUsageWithCost takes the fallback single-bucket path (bucketTokens)
// plus the subagent remainder bucket.
type fakeSubagentAwareAgent struct {
	mockBaseAgent

	usage *TokenUsage
}

func (f *fakeSubagentAwareAgent) ExtractAllModifiedFiles([]byte, int, string) ([]string, error) {
	return nil, nil
}

func (f *fakeSubagentAwareAgent) CalculateTotalTokenUsage([]byte, int, string) (*TokenUsage, error) { //nolint:unparam // test mock: error result satisfies SubagentAwareExtractor
	return f.usage, nil
}

func ptrFloat(v float64) *float64 { return &v }

// testTable returns a pricing table with two clean-rate override models on top
// of the embedded defaults: test-a ($1/MTok in&out) and test-b ($2/MTok in&out).
func testTable(t *testing.T) *pricing.Table {
	t.Helper()
	table, err := pricing.LoadTable([]pricing.ModelRate{
		{ID: "test-a", Provider: "test", InputPerMTok: 1, OutputPerMTok: 1},
		{ID: "test-b", Provider: "test", InputPerMTok: 2, OutputPerMTok: 2},
	})
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	return table
}

func TestCalculateUsageWithCost_FallbackBucketPriced(t *testing.T) {
	t.Parallel()
	ag := &fakeTokenCalcAgent{usage: &TokenUsage{InputTokens: 1_000_000}}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-a", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	if buckets[0].Model != "test-a" {
		t.Errorf("bucket model = %q, want test-a", buckets[0].Model)
	}
	if flat.CostUSD == nil || *flat.CostUSD != 1.0 {
		t.Fatalf("flat cost = %v, want 1.0", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceEstimated {
		t.Errorf("flat source = %q, want estimated", flat.CostSource)
	}
	if buckets[0].TokenUsage.CostUSD == nil || *buckets[0].TokenUsage.CostUSD != 1.0 {
		t.Errorf("bucket cost = %v, want 1.0", buckets[0].TokenUsage.CostUSD)
	}
}

func TestCalculateUsageWithCost_TwoModelsSumEstimated(t *testing.T) {
	t.Parallel()
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
			{Model: "test-b", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flat.CostUSD == nil || *flat.CostUSD != 3.0 {
		t.Fatalf("flat cost = %v, want 3.0 (1+2)", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceEstimated {
		t.Errorf("flat source = %q, want estimated", flat.CostSource)
	}
	if buckets[0].TokenUsage.CostUSD == nil || *buckets[0].TokenUsage.CostUSD != 1.0 {
		t.Errorf("bucket a cost = %v, want 1.0", buckets[0].TokenUsage.CostUSD)
	}
	if buckets[1].TokenUsage.CostUSD == nil || *buckets[1].TokenUsage.CostUSD != 2.0 {
		t.Errorf("bucket b cost = %v, want 2.0", buckets[1].TokenUsage.CostUSD)
	}
}

func TestCalculateUsageWithCost_UnknownModelMixed(t *testing.T) {
	t.Parallel()
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
			{Model: "who-knows", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flat.CostUSD == nil || *flat.CostUSD != 1.0 {
		t.Fatalf("flat cost = %v, want 1.0 (only test-a priced)", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceMixed {
		t.Errorf("flat source = %q, want mixed", flat.CostSource)
	}
	if buckets[1].TokenUsage.CostUSD != nil {
		t.Errorf("unknown bucket cost = %v, want nil", buckets[1].TokenUsage.CostUSD)
	}
}

func TestCalculateUsageWithCost_RemainderBucketPriced(t *testing.T) {
	t.Parallel()
	// Real extractors report the MAIN-transcript totals in the flat scalar fields
	// and the subagent totals in a SubagentTokens subtree, while CalculateModelUsage
	// only ever sees the main transcript. So the subagent usage surfaces solely as
	// a shortfall between the flattened flat total (main + subagents) and the
	// per-model bucket sum, and must be priced under fallbackModel. Here: main 1M
	// under test-a, subagent 1M must land in a test-b remainder bucket.
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{
			InputTokens:    1_000_000, // main transcript
			APICallCount:   3,
			SubagentTokens: &TokenUsage{InputTokens: 1_000_000, APICallCount: 4}, // subagents
		},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, APICallCount: 3}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-b", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2 (test-a + remainder)", len(buckets))
	}
	rem := buckets[1]
	if rem.Model != "test-b" {
		t.Errorf("remainder bucket model = %q, want test-b", rem.Model)
	}
	if rem.TokenUsage.InputTokens != 1_000_000 {
		t.Errorf("remainder input = %d, want 1_000_000 (subagent shortfall)", rem.TokenUsage.InputTokens)
	}
	if rem.TokenUsage.APICallCount != 4 {
		t.Errorf("remainder api_call_count = %d, want 4 (subagent api calls)", rem.TokenUsage.APICallCount)
	}
	if rem.TokenUsage.CostUSD == nil || *rem.TokenUsage.CostUSD != 2.0 {
		t.Errorf("remainder cost = %v, want 2.0 (1M @ $2/MTok)", rem.TokenUsage.CostUSD)
	}
	// F7/F8: no returned bucket may carry a SubagentTokens subtree.
	for i := range buckets {
		if buckets[i].TokenUsage.SubagentTokens != nil {
			t.Errorf("bucket %d carries SubagentTokens, want nil", i)
		}
	}
	// test-a 1M@$1 = 1.0, remainder 1M@$2 = 2.0 → 3.0, all estimated.
	if flat.CostUSD == nil || *flat.CostUSD != 3.0 {
		t.Fatalf("flat cost = %v, want 3.0", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceEstimated {
		t.Errorf("flat source = %q, want estimated", flat.CostSource)
	}
}

func TestCalculateUsageWithCost_RemainderBucketInflatedScalar(t *testing.T) {
	t.Parallel()
	// A ModelUsageCalculator whose buckets under-cover the flat SCALAR total (no
	// subagent subtree involved) still yields a remainder for the shortfall.
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000, APICallCount: 7},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, APICallCount: 3}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-b", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2 (test-a + remainder)", len(buckets))
	}
	rem := buckets[1]
	if rem.TokenUsage.InputTokens != 1_000_000 {
		t.Errorf("remainder input = %d, want 1_000_000", rem.TokenUsage.InputTokens)
	}
	if rem.TokenUsage.APICallCount != 4 {
		t.Errorf("remainder api_call_count = %d, want 4 (7 flat - 3 bucketed)", rem.TokenUsage.APICallCount)
	}
	if flat.CostUSD == nil || *flat.CostUSD != 3.0 {
		t.Fatalf("flat cost = %v, want 3.0", flat.CostUSD)
	}
}

func TestCalculateUsageWithCost_RemainderBucketNestedSubagents(t *testing.T) {
	t.Parallel()
	// Subagents that spawn subagents: the flat total's SubagentTokens nests two
	// levels deep. flattenTokenUsage must sum EVERY level so the whole subtree is
	// priced in the remainder bucket, not just the first.
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{
			InputTokens:  1_000_000, // main
			APICallCount: 1,
			SubagentTokens: &TokenUsage{
				InputTokens:    1_000_000, // depth-1 subagent
				APICallCount:   1,
				SubagentTokens: &TokenUsage{InputTokens: 1_000_000, APICallCount: 1}, // depth-2 subagent
			},
		},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, APICallCount: 1}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-b", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(buckets))
	}
	rem := buckets[1]
	if rem.TokenUsage.InputTokens != 2_000_000 {
		t.Errorf("remainder input = %d, want 2_000_000 (both nested subagent levels)", rem.TokenUsage.InputTokens)
	}
	if rem.TokenUsage.APICallCount != 2 {
		t.Errorf("remainder api_call_count = %d, want 2 (both subagent levels)", rem.TokenUsage.APICallCount)
	}
	// test-a 1M@$1 = 1.0, remainder 2M@$2 = 4.0 → 5.0.
	if flat.CostUSD == nil || *flat.CostUSD != 5.0 {
		t.Fatalf("flat cost = %v, want 5.0", flat.CostUSD)
	}
}

func TestCalculateUsageWithCost_SubagentRemainderFallbackNoAliasing(t *testing.T) {
	t.Parallel()
	// SubagentAware but NOT ModelUsage: the flat total carries a subagent subtree
	// and the single bucket comes from the fallback path (bucketTokens). The
	// fallback bucket must carry main-scoped scalars only (nil SubagentTokens) and
	// the subagent tokens must land in the remainder bucket — never aliased, so
	// two runs cannot inflate and the agent's own usage is left untouched.
	ag := &fakeSubagentAwareAgent{usage: &TokenUsage{
		InputTokens:    1_000_000, // main
		APICallCount:   2,
		SubagentTokens: &TokenUsage{InputTokens: 1_000_000, APICallCount: 3}, // subagent
	}}

	run := func() ([]types.ModelUsage, *types.TokenUsage) {
		flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "subdir", testTable(t), "test-a", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return buckets, flat
	}

	buckets1, flat1 := run()
	if len(buckets1) != 2 {
		t.Fatalf("buckets = %d, want 2 (fallback main bucket + subagent remainder)", len(buckets1))
	}
	for i := range buckets1 {
		if buckets1[i].TokenUsage.SubagentTokens != nil {
			t.Errorf("bucket %d carries SubagentTokens, want nil", i)
		}
	}
	if buckets1[0].TokenUsage.InputTokens != 1_000_000 {
		t.Errorf("main bucket input = %d, want 1_000_000", buckets1[0].TokenUsage.InputTokens)
	}
	rem := buckets1[1]
	if rem.TokenUsage.InputTokens != 1_000_000 || rem.TokenUsage.APICallCount != 3 {
		t.Errorf("remainder = %+v, want 1M input / 3 api (subagent)", rem.TokenUsage)
	}
	// main 1M@$1 + subagent 1M@$1 = 2.0.
	if flat1.CostUSD == nil || *flat1.CostUSD != 2.0 {
		t.Fatalf("flat cost = %v, want 2.0", flat1.CostUSD)
	}

	// A second run must yield identical token counts — no cross-call inflation.
	buckets2, _ := run()
	if buckets2[0].TokenUsage.InputTokens != buckets1[0].TokenUsage.InputTokens ||
		buckets2[1].TokenUsage.InputTokens != buckets1[1].TokenUsage.InputTokens {
		t.Fatalf("cross-call inflation: run1=[%d,%d] run2=[%d,%d]",
			buckets1[0].TokenUsage.InputTokens, buckets1[1].TokenUsage.InputTokens,
			buckets2[0].TokenUsage.InputTokens, buckets2[1].TokenUsage.InputTokens)
	}
	// The agent's own usage subtree must not have been mutated.
	if ag.usage.SubagentTokens.InputTokens != 1_000_000 {
		t.Fatalf("agent usage subtree mutated: %d, want 1_000_000", ag.usage.SubagentTokens.InputTokens)
	}
}

func TestCalculateUsageWithCost_RemainderUnpriceableFallbackMixed(t *testing.T) {
	t.Parallel()
	// Same shortfall, but the fallbackModel is not priceable. The remainder
	// bucket stays unpriced (tokens, no cost), so coverage folds to mixed.
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "who-knows", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2 (test-a + remainder)", len(buckets))
	}
	rem := buckets[1]
	if rem.Model != "who-knows" || rem.TokenUsage.InputTokens != 1_000_000 {
		t.Errorf("remainder bucket = %+v, want who-knows with 1M input", rem)
	}
	if rem.TokenUsage.CostUSD != nil {
		t.Errorf("remainder cost = %v, want nil (unpriceable)", rem.TokenUsage.CostUSD)
	}
	// Only test-a priced (1.0); remainder has tokens but no cost → mixed.
	if flat.CostUSD == nil || *flat.CostUSD != 1.0 {
		t.Fatalf("flat cost = %v, want 1.0 (test-a only)", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceMixed {
		t.Errorf("flat source = %q, want mixed", flat.CostSource)
	}
}

func TestCalculateUsageWithCost_NoRemainderWhenBucketsSumToFlat(t *testing.T) {
	t.Parallel()
	// Buckets already sum to the flat total: no remainder bucket is appended.
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
			{Model: "test-b", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
		},
	}

	_, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-a", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2 (no remainder appended)", len(buckets))
	}
}

func TestCalculateUsageWithCost_ReportedKeptEstimationOff(t *testing.T) {
	t.Parallel()
	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, CostUSD: ptrFloat(5.0), CostSource: types.CostSourceReported}},
			{Model: "test-b", TokenUsage: TokenUsage{InputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "", true /* disableEstimation */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Reported bucket survives untouched.
	if buckets[0].TokenUsage.CostUSD == nil || *buckets[0].TokenUsage.CostUSD != 5.0 {
		t.Errorf("reported bucket cost = %v, want 5.0", buckets[0].TokenUsage.CostUSD)
	}
	if buckets[0].TokenUsage.CostSource != types.CostSourceReported {
		t.Errorf("reported bucket source = %q, want reported", buckets[0].TokenUsage.CostSource)
	}
	// Estimation is off, so the priceable bucket stays unpriced.
	if buckets[1].TokenUsage.CostUSD != nil {
		t.Errorf("estimated-off bucket cost = %v, want nil", buckets[1].TokenUsage.CostUSD)
	}
	if flat.CostUSD == nil || *flat.CostUSD != 5.0 {
		t.Errorf("flat cost = %v, want 5.0 (reported only)", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceMixed {
		t.Errorf("flat source = %q, want mixed (reported + unpriced tokens)", flat.CostSource)
	}
}

func TestCalculateUsageWithCost_NilTableNoCost(t *testing.T) {
	t.Parallel()
	ag := &fakeTokenCalcAgent{usage: &TokenUsage{InputTokens: 1_000_000}}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", nil /* table */, "test-a", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flat.CostUSD != nil {
		t.Errorf("flat cost = %v, want nil", flat.CostUSD)
	}
	if flat.CostSource != "" {
		t.Errorf("flat source = %q, want empty", flat.CostSource)
	}
	if buckets[0].TokenUsage.CostUSD != nil {
		t.Errorf("bucket cost = %v, want nil", buckets[0].TokenUsage.CostUSD)
	}
}

func TestCalculateUsageWithCost_NoUsage(t *testing.T) {
	t.Parallel()
	// Agent that supports neither TokenCalculator nor ModelUsageCalculator.
	flat, buckets, err := CalculateUsageWithCost(&mockBaseAgent{}, nil, 0, "", testTable(t), "test-a", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flat != nil || buckets != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", flat, buckets)
	}
}

func TestCalculateUsageWithCost_FlatError(t *testing.T) {
	t.Parallel()
	ag := &fakeTokenCalcAgent{err: errors.New("boom")}
	if _, _, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-a", false); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCalculateUsageWithCost_ZeroTokenBucketUnpriced(t *testing.T) {
	t.Parallel()
	// A non-ModelUsageCalculator agent whose usage carries no billable tokens
	// (only APICallCount) takes the fallback single-bucket path under a resolvable
	// fallbackModel. Estimating that bucket would fabricate a $0.00 "estimated"
	// cost; instead it must stay unpriced (nil cost, empty source) so a zero-usage
	// checkpoint reads as "no cost data", consistent with the ModelUsage path.
	ag := &fakeTokenCalcAgent{usage: &TokenUsage{APICallCount: 5}}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", testTable(t), "test-a", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1 (fallback bucket, no remainder)", len(buckets))
	}
	if buckets[0].Model != "test-a" {
		t.Errorf("bucket model = %q, want test-a", buckets[0].Model)
	}
	if buckets[0].TokenUsage.CostUSD != nil {
		t.Errorf("zero-token bucket cost = %v, want nil (no $0 estimate)", buckets[0].TokenUsage.CostUSD)
	}
	if buckets[0].TokenUsage.CostSource != "" {
		t.Errorf("zero-token bucket source = %q, want empty", buckets[0].TokenUsage.CostSource)
	}
	if flat.CostUSD != nil {
		t.Errorf("flat cost = %v, want nil", flat.CostUSD)
	}
	if flat.CostSource != "" {
		t.Errorf("flat source = %q, want empty (not estimated)", flat.CostSource)
	}
}

func TestPriceUsage_ZeroTokenUsageMergesAsReported(t *testing.T) {
	t.Parallel()
	// End-to-end coupling for the zero-token guard: a zero-usage checkpoint priced
	// under a resolvable model must come back unpriced (nil cost, empty source),
	// so that when it is later aggregated with a reported checkpoint the merged
	// provenance stays "reported" instead of flipping to "mixed" — which a
	// fabricated $0.00 "estimated" figure would have caused.
	zero, _ := PriceUsage(&TokenUsage{APICallCount: 4}, "test-a", testTable(t), false)
	if zero.CostUSD != nil {
		t.Fatalf("zero-usage priced cost = %v, want nil (no $0 estimate)", zero.CostUSD)
	}
	if zero.CostSource != "" {
		t.Fatalf("zero-usage priced source = %q, want empty", zero.CostSource)
	}

	reported := &TokenUsage{InputTokens: 1_000_000, CostUSD: ptrFloat(6.25), CostSource: types.CostSourceReported}
	if got := types.MergeCostSourceUsages(reported, zero); got != types.CostSourceReported {
		t.Errorf("merge = %q, want reported (zero-usage checkpoint must not flip provenance)", got)
	}
	if got := types.MergeCostSourceUsages(zero, reported); got != types.CostSourceReported {
		t.Errorf("merge (swapped) = %q, want reported", got)
	}
}

func TestPriceUsage_EstimatesUnderModel(t *testing.T) {
	t.Parallel()
	priced, buckets := PriceUsage(&TokenUsage{InputTokens: 1_000_000}, "test-b", testTable(t), false)
	if priced.CostUSD == nil || *priced.CostUSD != 2.0 {
		t.Fatalf("cost = %v, want 2.0", priced.CostUSD)
	}
	if priced.CostSource != types.CostSourceEstimated {
		t.Errorf("source = %q, want estimated", priced.CostSource)
	}
	if len(buckets) != 1 || buckets[0].Model != "test-b" {
		t.Errorf("buckets = %+v, want single test-b", buckets)
	}
}

func TestPriceUsage_KeepsReported(t *testing.T) {
	t.Parallel()
	in := &TokenUsage{InputTokens: 1_000_000, CostUSD: ptrFloat(9.0), CostSource: types.CostSourceReported}
	priced, _ := PriceUsage(in, "test-a", testTable(t), false)
	if priced.CostUSD == nil || *priced.CostUSD != 9.0 {
		t.Fatalf("cost = %v, want 9.0 (reported kept)", priced.CostUSD)
	}
	if priced.CostSource != types.CostSourceReported {
		t.Errorf("source = %q, want reported", priced.CostSource)
	}
}

func TestPriceUsage_NilUsage(t *testing.T) {
	t.Parallel()
	priced, buckets := PriceUsage(nil, "test-a", testTable(t), false)
	if priced != nil || buckets != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", priced, buckets)
	}
}

// TestCalculateUsageWithCost_ClaudeFastVariantPricedAtPremium proves the
// claudecode fast-mode buckets (Model "<model>-fast") price at the fast premium
// through the REAL embedded table: claude-opus-4-8-fast bills at 10/50, double
// the standard claude-opus-4-8 5/25. The bucket keys themselves are produced by
// claudecode.CalculateModelUsage (covered in the claudecode package, which must
// not import pricing); this is the priced half of that feature.
func TestCalculateUsageWithCost_ClaudeFastVariantPricedAtPremium(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}

	ag := &fakeModelUsageAgent{
		flat: &TokenUsage{InputTokens: 2_000_000, OutputTokens: 2_000_000},
		buckets: []types.ModelUsage{
			{Model: "claude-opus-4-8", TokenUsage: TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
			{Model: "claude-opus-4-8-fast", TokenUsage: TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
		},
	}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "", table, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// standard: 1M@$5 + 1M@$25 = 30.0
	if buckets[0].Model != "claude-opus-4-8" || buckets[0].TokenUsage.CostUSD == nil || *buckets[0].TokenUsage.CostUSD != 30.0 {
		t.Errorf("standard bucket = %+v (cost %v), want claude-opus-4-8 @ 30.0", buckets[0].Model, buckets[0].TokenUsage.CostUSD)
	}
	// fast: 1M@$10 + 1M@$50 = 60.0 (double the standard)
	if buckets[1].Model != "claude-opus-4-8-fast" || buckets[1].TokenUsage.CostUSD == nil || *buckets[1].TokenUsage.CostUSD != 60.0 {
		t.Errorf("fast bucket = %+v (cost %v), want claude-opus-4-8-fast @ 60.0", buckets[1].Model, buckets[1].TokenUsage.CostUSD)
	}
	if flat.CostUSD == nil || *flat.CostUSD != 90.0 {
		t.Fatalf("flat cost = %v, want 90.0", flat.CostUSD)
	}
	if flat.CostSource != types.CostSourceEstimated {
		t.Errorf("flat source = %q, want estimated", flat.CostSource)
	}
}

// TestRemainderBucket_APICallCountOnlyShortfallNoRemainder proves a shortfall
// that is only an APICallCount delta (all four token fields fully covered by
// the per-model buckets) does not produce a spurious zero-token remainder
// bucket.
func TestRemainderBucket_APICallCountOnlyShortfallNoRemainder(t *testing.T) {
	t.Parallel()
	flat := &TokenUsage{InputTokens: 1_000_000, APICallCount: 5}
	buckets := []types.ModelUsage{
		{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, APICallCount: 2}},
	}

	_, ok := remainderBucket(flat, buckets, "test-a")
	if ok {
		t.Fatalf("remainderBucket returned a bucket for an APICallCount-only shortfall, want none")
	}
}

// TestRemainderBucket_RealTokenShortfallStillReturned proves a genuine token
// shortfall (not just an APICallCount delta) still produces a remainder
// bucket, guarding against an overly broad fix to the APICallCount exclusion.
func TestRemainderBucket_RealTokenShortfallStillReturned(t *testing.T) {
	t.Parallel()
	flat := &TokenUsage{InputTokens: 2_000_000, APICallCount: 5}
	buckets := []types.ModelUsage{
		{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, APICallCount: 2}},
	}

	rem, ok := remainderBucket(flat, buckets, "test-a")
	if !ok {
		t.Fatalf("remainderBucket returned no bucket for a real token shortfall")
	}
	if rem.TokenUsage.InputTokens != 1_000_000 {
		t.Errorf("remainder InputTokens = %d, want 1_000_000", rem.TokenUsage.InputTokens)
	}
}

// TestFoldBucketCost_EmptySourceWithCostNormalizesToEstimated proves a bucket
// carrying a non-nil CostUSD but an empty CostSource (currently unreachable via
// priceBuckets, but defended against as a boundary case) folds into a total
// labeled CostSourceEstimated rather than an unlabeled "" source.
func TestFoldBucketCost_EmptySourceWithCostNormalizesToEstimated(t *testing.T) {
	t.Parallel()
	buckets := []types.ModelUsage{
		{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, CostUSD: ptrFloat(5.0), CostSource: ""}},
	}

	cost, source := foldBucketCost(buckets)
	if cost == nil || *cost != 5.0 {
		t.Fatalf("cost = %v, want 5.0", cost)
	}
	if source != types.CostSourceEstimated {
		t.Errorf("source = %q, want estimated", source)
	}
}
