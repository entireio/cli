package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// A model with no verified ratio row must resolve to "no weights" rather than
// to a default. Guessing a model's pricing would put an invented cost share in
// front of the user; reporting volume only is the honest degradation.
func TestTokenWeightsForModel_UnknownModelHasNoWeights(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"", "  ", "gpt-4", "claude", "llama-3", "some-future-model"} {
		if _, ok := tokenWeightsForModel(model); ok {
			t.Errorf("tokenWeightsForModel(%q) returned weights; an unverified model must report none", model)
		}
	}
}

func TestTokenWeightsForModel_KnownFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model      string
		wantOutput float64
		wantWrite  bool // provider charges for cache writes
	}{
		{"claude-sonnet-4.6", 5, true},
		{"claude-opus-4.6[1m]", 5, true},
		{"gpt-5.3-codex", 8, false},
		{"gpt-5.5", 6, false},
		{"gpt-5.6-sol", 5, true},
		{"gemini-2.5-pro", 8, false},
		{"gemini-3.6-flash", 5, false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			w, ok := tokenWeightsForModel(tt.model)
			if !ok {
				t.Fatalf("tokenWeightsForModel(%q) = not found, want a verified row", tt.model)
			}
			if w.Input != 1 {
				t.Errorf("Input ratio = %v, want 1 (every row is relative to its own base input price)", w.Input)
			}
			if w.Output != tt.wantOutput {
				t.Errorf("Output ratio = %v, want %v", w.Output, tt.wantOutput)
			}
			if got := w.CacheWrite5m > 0; got != tt.wantWrite {
				t.Errorf("charges for cache writes = %v, want %v", got, tt.wantWrite)
			}
		})
	}
}

// Anthropic's 1-hour cache TTL bills at 2× against 1.25× for the 5-minute one.
// That difference is the whole reason cache_creation_1h_tokens is persisted, so
// the two ratios must not collapse to one.
func TestTokenWeightsForModel_AnthropicSeparatesCacheTTLs(t *testing.T) {
	t.Parallel()

	w, ok := tokenWeightsForModel("claude-sonnet-4.6")
	if !ok {
		t.Fatal("claude-sonnet-4.6 must have a verified row")
	}
	if w.CacheWrite1h <= w.CacheWrite5m {
		t.Errorf("CacheWrite1h (%v) must cost more than CacheWrite5m (%v)", w.CacheWrite1h, w.CacheWrite5m)
	}
}

// Volume shares must sum to exactly 100 — a report that prints 99% or 101% is
// visibly wrong to the reader.
func TestTokenClassShares_VolumeSumsTo100(t *testing.T) {
	t.Parallel()

	// Three equal parts of a whole: 33.33% each, so the naive floors sum to 99
	// and the largest-remainder correction has to close the gap. {1,1,1,1} would
	// NOT test this — it divides into four exact 25s and never enters the
	// correction at all.
	usage := &types.TokenUsage{
		InputTokens:         1,
		CacheCreationTokens: 1,
		CacheReadTokens:     1,
	}

	shares, ok := tokenClassShares(usage, tokenWeights{}, false)
	if !ok {
		t.Fatal("usage with tokens must produce shares")
	}
	total := shares.Input.VolumePercent + shares.CacheWrite.VolumePercent +
		shares.CacheRead.VolumePercent + shares.Output.VolumePercent
	if total != 100 {
		t.Errorf("volume shares sum to %d, want exactly 100", total)
	}
}

func TestTokenClassShares_CostSumsTo100WhenPriced(t *testing.T) {
	t.Parallel()

	w, ok := tokenWeightsForModel("claude-sonnet-4.6")
	if !ok {
		t.Fatal("claude-sonnet-4.6 must have a verified row")
	}
	usage := &types.TokenUsage{
		InputTokens:         1234,
		CacheCreationTokens: 5678,
		CacheReadTokens:     91011,
		OutputTokens:        1213,
	}

	shares, ok := tokenClassShares(usage, w, true)
	if !ok {
		t.Fatal("priced usage must produce shares")
	}
	if !shares.Priced {
		t.Fatal("shares must be marked priced when weights are supplied")
	}
	total := shares.Input.CostPercent + shares.CacheWrite.CostPercent +
		shares.CacheRead.CostPercent + shares.Output.CostPercent
	if total != 100 {
		t.Errorf("cost shares sum to %d, want exactly 100", total)
	}
}

// Without a model there are no ratios, so the report shows volume only. Cost
// must be absent rather than zero — zero reads as "this cost nothing".
func TestTokenClassShares_NoWeightsMeansNoCost(t *testing.T) {
	t.Parallel()

	usage := &types.TokenUsage{InputTokens: 100, CacheReadTokens: 900}

	shares, ok := tokenClassShares(usage, tokenWeights{}, false)
	if !ok {
		t.Fatal("usage with tokens must produce shares")
	}
	if shares.Priced {
		t.Error("shares must not be marked priced without weights")
	}
	if shares.Input.CostPercent != 0 || shares.CacheRead.CostPercent != 0 {
		t.Error("cost percents must be zero-valued and unused when unpriced")
	}
	if shares.Input.VolumePercent+shares.CacheRead.VolumePercent != 100 {
		t.Error("volume shares must still be exact without a model")
	}
}

// The cache-write TTL split is only knowable on token_usage_version 2 rows.
// When it is unknown the writes must not be priced at a guessed TTL.
func TestTokenClassShares_UnknownTTLIsNotPriced(t *testing.T) {
	t.Parallel()

	w, _ := tokenWeightsForModel("claude-sonnet-4.6")
	usage := &types.TokenUsage{InputTokens: 100, CacheCreationTokens: 100, OutputTokens: 100}

	if shares, ok := tokenClassShares(usage, w, false); !ok || shares.Priced {
		t.Error("cache writes with an unknown TTL must not be priced")
	}
}

// Thinking and 1-hour cache writes are subsets of output and cache-write
// respectively. Counting them as their own class would double-count the total.
func TestTokenClassShares_SubsetsAreNotClasses(t *testing.T) {
	t.Parallel()

	usage := &types.TokenUsage{
		InputTokens:           10,
		CacheCreationTokens:   100,
		CacheCreation1hTokens: 40,
		CacheReadTokens:       10,
		OutputTokens:          80,
		ThinkingTokens:        30,
	}

	shares, ok := tokenClassShares(usage, tokenWeights{}, false)
	if !ok {
		t.Fatal("usage with tokens must produce shares")
	}
	if shares.Total != 200 {
		t.Errorf("Total = %d, want 200 (10+100+10+80; subsets excluded)", shares.Total)
	}
	if shares.CacheWrite.Tokens != 100 {
		t.Errorf("cache-write class = %d, want the full 100 including the 1h subset", shares.CacheWrite.Tokens)
	}
	if shares.CacheWrite1h != 40 || shares.Thinking != 30 {
		t.Error("subsets must be reported alongside their parent class")
	}
}

// A checkpoint that recorded nothing must say so rather than render four zeros,
// which reads as "this session was free".
func TestTokenClassShares_NoUsageIsNotRecorded(t *testing.T) {
	t.Parallel()

	if _, ok := tokenClassShares(nil, tokenWeights{}, false); ok {
		t.Error("nil usage must report no shares")
	}
	if _, ok := tokenClassShares(&types.TokenUsage{}, tokenWeights{}, false); ok {
		t.Error("all-zero usage must report no shares")
	}
}

// Checkpoint metadata lives on a branch anyone with push access can write. A
// negative count must not escape as a negative or >100 percentage.
func TestTokenClassShares_HostileNegativeCountsAreClamped(t *testing.T) {
	t.Parallel()

	shares, ok := tokenClassShares(&types.TokenUsage{InputTokens: -100, CacheReadTokens: 1000}, tokenWeights{}, false)
	if !ok {
		t.Fatal("expected shares")
	}
	for name, got := range map[string]int{
		"input":       shares.Input.VolumePercent,
		"cache read":  shares.CacheRead.VolumePercent,
		"cache write": shares.CacheWrite.VolumePercent,
		"output":      shares.Output.VolumePercent,
	} {
		if got < 0 || got > 100 {
			t.Errorf("%s share = %d%%, want within [0,100]", name, got)
		}
	}
	if shares.Input.Tokens < 0 {
		t.Errorf("input tokens = %d, want clamped at zero", shares.Input.Tokens)
	}
}

// Subagent usage is nested. The section above this one totals with recursion,
// so these classes must too or one report shows two different totals.
func TestTokenClassShares_FlattensSubagentUsage(t *testing.T) {
	t.Parallel()

	usage := &types.TokenUsage{
		InputTokens: 100, OutputTokens: 100,
		SubagentTokens: &types.TokenUsage{InputTokens: 300, OutputTokens: 500},
	}

	shares, ok := tokenClassShares(usage, tokenWeights{}, false)
	if !ok {
		t.Fatal("expected shares")
	}
	if shares.Total != 1000 {
		t.Errorf("Total = %d, want 1000 — subagent tokens were billed too", shares.Total)
	}
	if shares.Input.Tokens != 400 {
		t.Errorf("input = %d, want 400 (100 + 300 from the subagent)", shares.Input.Tokens)
	}
}

// A 1-hour figure larger than its parent class must not make the 5-minute
// remainder negative.
func TestTokenClassShares_ClampsOversized1hSubset(t *testing.T) {
	t.Parallel()

	w, _ := tokenWeightsForModel("claude-sonnet-4.6")
	shares, ok := tokenClassShares(
		&types.TokenUsage{InputTokens: 10, CacheCreationTokens: 100, CacheCreation1hTokens: 500, OutputTokens: 10}, w, true)
	if !ok {
		t.Fatal("expected shares")
	}
	total := shares.Input.CostPercent + shares.CacheWrite.CostPercent + shares.CacheRead.CostPercent + shares.Output.CostPercent
	if total != 100 {
		t.Errorf("cost shares sum to %d, want 100 even with an oversized 1h subset", total)
	}
}

// A legacy row with no cache writes has no TTL split to be ambiguous about, so
// withholding its whole cost column would be over-cautious.
func TestTokenClassShares_LegacyWithoutCacheWritesIsPriced(t *testing.T) {
	t.Parallel()

	w, _ := tokenWeightsForModel("claude-sonnet-4.6")
	shares, ok := tokenClassShares(&types.TokenUsage{InputTokens: 100, CacheReadTokens: 900, OutputTokens: 50}, w, false)
	if !ok {
		t.Fatal("expected shares")
	}
	if !shares.Priced {
		t.Errorf("no cache writes means no TTL ambiguity; want priced, reason was %q", shares.UnpricedReason)
	}
}

// Each withholding case must state its own reason. A report that misstates why
// it withheld a number is as wrong as withholding the wrong one.
func TestTokenClassShares_UnpricedReasonsAreSpecific(t *testing.T) {
	t.Parallel()

	anthropic, _ := tokenWeightsForModel("claude-sonnet-4.6")

	noModel, _ := tokenClassShares(&types.TokenUsage{InputTokens: 100}, tokenWeights{}, false)
	if noModel.UnpricedReason != unpricedNoModel {
		t.Errorf("no-model reason = %q, want %q", noModel.UnpricedReason, unpricedNoModel)
	}

	legacyTTL, _ := tokenClassShares(&types.TokenUsage{InputTokens: 100, CacheCreationTokens: 100}, anthropic, false)
	if legacyTTL.UnpricedReason != unpricedUnknownTTL {
		t.Errorf("legacy-TTL reason = %q, want %q — it must not claim the model has no ratios", legacyTTL.UnpricedReason, unpricedUnknownTTL)
	}
}
