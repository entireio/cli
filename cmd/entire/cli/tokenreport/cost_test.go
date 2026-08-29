package tokenreport

import (
	"math"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// assertClose fails when a and b differ by 1e-9 or more.
func assertClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) >= 1e-9 {
		t.Errorf("got %v, want %v", got, want)
	}
}

// assertCloseRel fails when got and want differ by more than 1e-12 relative
// to want; for magnitudes such as Units, where the absolute 1e-9 bound of
// assertClose only suits shares in [0, 1].
func assertCloseRel(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12*math.Abs(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// assertWeightsClose compares every ratio field of got with want to within
// assertClose's tolerance and the Provider/Family fields exactly.
func assertWeightsClose(t *testing.T, label string, got, want Weights) {
	t.Helper()
	if got.Provider != want.Provider || got.Family != want.Family {
		t.Errorf("%s: provider/family %q/%q, want %q/%q", label, got.Provider, got.Family, want.Provider, want.Family)
	}
	for field, pair := range map[string][2]float64{
		"Input":        {got.Input, want.Input},
		"CacheWrite5m": {got.CacheWrite5m, want.CacheWrite5m},
		"CacheWrite1h": {got.CacheWrite1h, want.CacheWrite1h},
		"CacheRead":    {got.CacheRead, want.CacheRead},
		"Output":       {got.Output, want.Output},
	} {
		if math.Abs(pair[0]-pair[1]) >= 1e-9 {
			t.Errorf("%s: %s = %v, want %v", label, field, pair[0], pair[1])
		}
	}
}

func TestWeightsFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model             string
		ok                bool
		out, cr, cw5, cw1 float64
		family            Family
		provider          Provider
	}{
		{"claude-fable-5", true, 5, 0.1, 1.25, 2, FamilyAnthropic, ProviderAnthropic},
		{"claude-opus-4-8[1m]", true, 5, 0.1, 1.25, 2, FamilyAnthropic, ProviderAnthropic}, // 1M context is standard pricing
		{"Claude-Sonnet-4-5", true, 5, 0.1, 1.25, 2, FamilyAnthropic, ProviderAnthropic},   // case-insensitive like formatModel
		{"  claude-haiku-4-5  ", true, 5, 0.1, 1.25, 2, FamilyAnthropic, ProviderAnthropic},
		{"gpt-5", true, 8, 0.1, 0, 0, FamilyOpenAI8x, ProviderOpenAI},
		{"gpt-5-mini", true, 8, 0.1, 0, 0, FamilyOpenAI8x, ProviderOpenAI},
		{"gpt-5.1", true, 8, 0.1, 0, 0, FamilyOpenAI8x, ProviderOpenAI},
		{"gpt-5.2-codex", true, 8, 0.1, 0, 0, FamilyOpenAI8x, ProviderOpenAI},
		{"gpt-5.3-codex", true, 8, 0.1, 0, 0, FamilyOpenAI8x, ProviderOpenAI},
		{"gpt-5.4", true, 6, 0.1, 0, 0, FamilyOpenAI6x, ProviderOpenAI},
		{"gpt-5.4-mini", true, 6, 0.1, 0, 0, FamilyOpenAI6x, ProviderOpenAI},
		{"gpt-5.5", true, 6, 0.1, 0, 0, FamilyOpenAI6x, ProviderOpenAI},
		{"gpt-5.6-sol", true, 5, 0.1, 1.25, 1.25, FamilyOpenAI56Sol, ProviderOpenAI},
		{"gpt-5.6-terra", true, 6, 0.1, 1.25, 1.25, FamilyOpenAI56TerraLuna, ProviderOpenAI},
		{"gpt-5.6-luna", true, 6, 0.1, 1.25, 1.25, FamilyOpenAI56TerraLuna, ProviderOpenAI},
		{"gpt-5.6", false, 0, 0, 0, 0, "", ""}, // unknown 5.6 variant: not priced
		{"gpt-4o", false, 0, 0, 0, 0, "", ""},
		{"gemini-2.5-flash", true, 8.33, 0.1, 0, 0, FamilyGemini25Flash, ProviderGoogle},
		{"gemini-2.5-pro", true, 8, 0.1, 0, 0, FamilyGemini25Pro, ProviderGoogle},
		{"gemini-3.5-flash", true, 6, 0.1, 0, 0, FamilyGemini35Flash, ProviderGoogle},
		{"gemini-3.6-flash", true, 5, 0.1, 0, 0, FamilyGemini36Flash, ProviderGoogle},
		{"gemini-3.7-flash", true, 5, 0.1, 0, 0, FamilyGemini36Flash, ProviderGoogle},
		{"gemini-3-flash-preview", false, 0, 0, 0, 0, "", ""},
		{"", false, 0, 0, 0, 0, "", ""},
		{"   ", false, 0, 0, 0, 0, "", ""},
	}
	for _, c := range cases {
		w, fam, ok := WeightsFor(c.model)
		if ok != c.ok || fam != c.family {
			t.Errorf("%q: ok=%v fam=%q, want ok=%v fam=%q", c.model, ok, fam, c.ok, c.family)
			continue
		}
		if !ok {
			if w != (Weights{}) {
				t.Errorf("%q: unknown model must return zero weights, got %+v", c.model, w)
			}
			continue
		}
		if w.Input != 1 {
			t.Errorf("%q: Input weight %v, want 1 (ratios are relative to input)", c.model, w.Input)
		}
		if w.Output != c.out || w.CacheRead != c.cr || w.CacheWrite5m != c.cw5 || w.CacheWrite1h != c.cw1 {
			t.Errorf("%q: weights %+v", c.model, w)
		}
		if w.Family != fam {
			t.Errorf("%q: weights family %q, want %q", c.model, w.Family, fam)
		}
		if w.Provider != c.provider {
			t.Errorf("%q: weights provider %q, want %q", c.model, w.Provider, c.provider)
		}
	}
}

func TestProviderOf(t *testing.T) {
	t.Parallel()

	cases := map[Family]Provider{
		FamilyAnthropic:         ProviderAnthropic,
		FamilyOpenAI8x:          ProviderOpenAI,
		FamilyOpenAI6x:          ProviderOpenAI,
		FamilyOpenAI56Sol:       ProviderOpenAI,
		FamilyOpenAI56TerraLuna: ProviderOpenAI,
		FamilyGemini25Flash:     ProviderGoogle,
		FamilyGemini25Pro:       ProviderGoogle,
		FamilyGemini35Flash:     ProviderGoogle,
		FamilyGemini36Flash:     ProviderGoogle,
		"":                      "",
		"unknown-family":        "",
	}
	for fam, want := range cases {
		if got := ProviderOf(fam); got != want {
			t.Errorf("ProviderOf(%q) = %q, want %q", fam, got, want)
		}
	}
}

func TestComputeCostShares_AnthropicWorkedExample(t *testing.T) {
	t.Parallel()

	// grounding mock: in 6.5k, cw 332.9k all 1h, cr 3.7M, out 115.1k, thinking 65.3k
	u := &types.TokenUsage{
		InputTokens:           6500,
		CacheCreationTokens:   332900,
		CacheCreation1hTokens: 332900,
		CacheReadTokens:       3_700_000,
		OutputTokens:          115100,
		ThinkingTokens:        65300,
	}
	w, fam, _ := WeightsFor("claude-fable-5")
	cs := ComputeCostShares(u, w)
	// units = 6500 + 332900*2 + 3.7M*0.1 + 115100*5 = 6500+665800+370000+575500 = 1,617,800
	if cs.Units != 1617800 {
		t.Errorf("units %v", cs.Units)
	}
	assertClose(t, cs.Input, 6500.0/1617800)
	assertClose(t, cs.CacheWrite, 665800.0/1617800)
	assertClose(t, cs.CacheRead, 370000.0/1617800)
	assertClose(t, cs.Output, 575500.0/1617800)
	assertClose(t, cs.Thinking, 65300*5.0/1617800)
	assertClose(t, cs.Input+cs.CacheWrite+cs.CacheRead+cs.Output, 1)
	if cs.CacheWriteUnpriced {
		t.Error("cache write must be priced when the 1h split is recorded")
	}
	if cs.Provider != ProviderAnthropic || cs.Family != fam {
		t.Errorf("provider/family not copied from weights: %+v", cs)
	}
}

func TestComputeCostShares_AnthropicMixedTTL(t *testing.T) {
	t.Parallel()

	// 1000 cache-write tokens, 400 of them 1h: 400*2 + 600*1.25 = 1550 units.
	u := &types.TokenUsage{CacheCreationTokens: 1000, CacheCreation1hTokens: 400, OutputTokens: 100}
	w, _, _ := WeightsFor("claude-sonnet-5")
	cs := ComputeCostShares(u, w)
	if cs.Units != 1550+500 {
		t.Errorf("units %v", cs.Units)
	}
	assertClose(t, cs.CacheWrite, 1550.0/2050)
	if cs.CacheWriteUnpriced {
		t.Error("cache write must be priced when the 1h split is recorded")
	}
}

func TestComputeCostShares_LegacyAnthropicCacheWriteUnpriced(t *testing.T) {
	t.Parallel()

	u := &types.TokenUsage{CacheCreationTokens: 1000, OutputTokens: 100} // no 1h field → TTL unknown
	w, _, _ := WeightsFor("claude-sonnet-5")
	cs := ComputeCostShares(u, w)
	if !cs.CacheWriteUnpriced || cs.CacheWrite != 0 {
		t.Errorf("legacy cache write must be unpriced: %+v", cs)
	}
	// Units exclude the unpriced cache write: only the 100 output tokens at 5×.
	if cs.Units != 500 {
		t.Errorf("units %v, want 500", cs.Units)
	}
	assertClose(t, cs.Output, 1)
}

func TestComputeCostSharesKnownTTL_ZeroOneHourMeansAllFiveMinute(t *testing.T) {
	t.Parallel()

	u := &types.TokenUsage{CacheCreationTokens: 1000, OutputTokens: 100} // per-call block: 1h really is 0
	w, _, _ := WeightsFor("claude-sonnet-5")
	legacy := ComputeCostShares(u, w)
	known := ComputeCostSharesKnownTTL(u, w)
	if !legacy.CacheWriteUnpriced {
		t.Errorf("metadata variant must stay unpriced: %+v", legacy)
	}
	if known.CacheWriteUnpriced {
		t.Errorf("known-TTL variant must never be unpriced: %+v", known)
	}
	// 1000 writes at the 5m rate 1.25× + 100 output at 5×.
	assertCloseRel(t, known.Units, 1250+500)
	assertClose(t, known.CacheWrite, 1250.0/1750)
	assertClose(t, known.Output, 500.0/1750)
}

func TestComputeCostSharesKnownTTL_AgreesWhenTTLIsRecordedOrSinglePriced(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		u     *types.TokenUsage
		model string
	}{
		"anthropic all 1h":  {&types.TokenUsage{CacheCreationTokens: 1000, CacheCreation1hTokens: 1000, OutputTokens: 100}, "claude-sonnet-5"},
		"anthropic mixed":   {&types.TokenUsage{CacheCreationTokens: 1000, CacheCreation1hTokens: 400, OutputTokens: 100}, "claude-sonnet-5"},
		"openai single TTL": {&types.TokenUsage{CacheCreationTokens: 1000, OutputTokens: 100}, "gpt-5.6-sol"},
		"gemini no charge":  {&types.TokenUsage{CacheCreationTokens: 1000, OutputTokens: 100}, "gemini-2.5-pro"},
		"nil":               {nil, "claude-sonnet-5"},
	}
	for name, tc := range cases {
		w, _, _ := WeightsFor(tc.model)
		if got, want := ComputeCostSharesKnownTTL(tc.u, w), ComputeCostShares(tc.u, w); got != want {
			t.Errorf("%s: known %+v != metadata %+v", name, got, want)
		}
	}
}

func TestComputeCostShares_SingleTTLFamilyNeverUnpriced(t *testing.T) {
	t.Parallel()

	// gpt-5.6-sol charges one cache-write rate, so the missing 1h split is
	// irrelevant: 1000 * 1.25 = 1250 units.
	u := &types.TokenUsage{CacheCreationTokens: 1000, OutputTokens: 100}
	w, _, _ := WeightsFor("gpt-5.6-sol")
	cs := ComputeCostShares(u, w)
	if cs.CacheWriteUnpriced {
		t.Errorf("single-rate family must not flag unpriced: %+v", cs)
	}
	if cs.Units != 1250+500 {
		t.Errorf("units %v", cs.Units)
	}
	assertClose(t, cs.CacheWrite, 1250.0/1750)

	// Families with no cache-write charge at all price it as zero, not unpriced.
	w, _, _ = WeightsFor("gpt-5.4")
	cs = ComputeCostShares(u, w)
	if cs.CacheWriteUnpriced || cs.CacheWrite != 0 || cs.Units != 600 {
		t.Errorf("zero-rate family: %+v", cs)
	}
}

func TestComputeCostShares_ZeroUsage(t *testing.T) {
	t.Parallel()

	w, _, _ := WeightsFor("claude-fable-5")
	for _, u := range []*types.TokenUsage{nil, {}} {
		cs := ComputeCostShares(u, w)
		if cs.Units != 0 || cs.Input != 0 || cs.CacheWrite != 0 || cs.CacheRead != 0 || cs.Output != 0 || cs.Thinking != 0 {
			t.Errorf("zero usage must yield zero shares (no NaN): %+v", cs)
		}
		if cs.CacheWriteUnpriced {
			t.Errorf("zero cache write is not unpriced: %+v", cs)
		}
		if cs.Provider != ProviderAnthropic || cs.Family != FamilyAnthropic {
			t.Errorf("provider/family must still be copied: %+v", cs)
		}
	}
}

func TestWeightsForCall_LongContextTierPerCall(t *testing.T) {
	t.Parallel()

	openAI6x := Weights{Provider: ProviderOpenAI, Family: FamilyOpenAI6x, Input: 1, CacheRead: 0.1, Output: 6}
	sol := Weights{Provider: ProviderOpenAI, Family: FamilyOpenAI56Sol, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 5}
	terraLuna := Weights{Provider: ProviderOpenAI, Family: FamilyOpenAI56TerraLuna, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 6}
	geminiPro := Weights{Provider: ProviderGoogle, Family: FamilyGemini25Pro, Input: 1, CacheRead: 0.1, Output: 8}

	cases := []struct {
		label       string
		model       string
		inputTokens int
		want        Weights
	}{
		// gpt-5.5 call with 300k input: input & cached 2×, output 1.5×.
		{"gpt-5.5 long context", "gpt-5.5", 300_000, Weights{Provider: ProviderOpenAI, Family: FamilyOpenAI6x, Input: 2, CacheRead: 0.2, Output: 9}},
		// Exactly at the threshold is still the base tier.
		{"gpt-5.5 at threshold", "gpt-5.5", 272_000, openAI6x},
		// gpt-5.6-sol: 2× input, 2× cached, 2× write, 1.5× output.
		{"sol long context", "gpt-5.6-sol", 272_001, Weights{Provider: ProviderOpenAI, Family: FamilyOpenAI56Sol, Input: 2, CacheWrite5m: 2.5, CacheWrite1h: 2.5, CacheRead: 0.2, Output: 7.5}},
		{"sol at threshold", "gpt-5.6-sol", 272_000, sol},
		// gpt-5.6-terra: same shape on a 6× base.
		{"terra long context", "gpt-5.6-terra", 300_000, Weights{Provider: ProviderOpenAI, Family: FamilyOpenAI56TerraLuna, Input: 2, CacheWrite5m: 2.5, CacheWrite1h: 2.5, CacheRead: 0.2, Output: 9}},
		{"terra at threshold", "gpt-5.6-terra", 272_000, terraLuna},
		// gemini-2.5-pro over 200k: 2× input, 2× cached, 1.5× output.
		{"gemini pro long context", "gemini-2.5-pro", 200_001, Weights{Provider: ProviderGoogle, Family: FamilyGemini25Pro, Input: 2, CacheRead: 0.2, Output: 12}},
		{"gemini pro at threshold", "gemini-2.5-pro", 200_000, geminiPro},
	}
	for _, c := range cases {
		assertWeightsClose(t, c.label, WeightsForCall(c.model, c.inputTokens), c.want)
	}
}

func TestWeightsForCall_NoTierFamilies(t *testing.T) {
	t.Parallel()

	// Anthropic (1M context is standard pricing), 8× OpenAI, and Gemini
	// flash have no long-context tier: any input size yields the base weights.
	for _, model := range []string{"claude-opus-4-8[1m]", "gpt-5.3-codex", "gemini-2.5-flash", "gemini-3.5-flash"} {
		base, _, ok := WeightsFor(model)
		if !ok {
			t.Fatalf("%s: expected known model", model)
		}
		if got := WeightsForCall(model, 5_000_000); got != base {
			t.Errorf("%s: long input changed weights: %+v vs %+v", model, got, base)
		}
	}
	// Unknown model: zero weights regardless of input.
	if got := WeightsForCall("gemini-3-flash-preview", 5_000_000); got != (Weights{}) {
		t.Errorf("unknown model: %+v", got)
	}
}

func TestSumCostShares_SameFamilyPropagatesUnpriced(t *testing.T) {
	t.Parallel()

	a := CostShares{Provider: ProviderAnthropic, Family: FamilyAnthropic, Units: 1000, Output: 0.5, CacheRead: 0.5}           // 500 output units, 500 cache-read units
	b := CostShares{Provider: ProviderAnthropic, Family: FamilyAnthropic, Units: 3000, Output: 1.0, CacheWriteUnpriced: true} // 3000 output units
	got := SumCostShares(a, b)
	if got.Units != 4000 {
		t.Errorf("units %v", got.Units)
	}
	assertClose(t, got.Output, 3500.0/4000)
	assertClose(t, got.CacheRead, 500.0/4000)
	assertClose(t, got.Input, 0)
	assertClose(t, got.CacheWrite, 0)
	if !got.CacheWriteUnpriced {
		t.Error("unpriced flag must propagate")
	}
	if got.Provider != ProviderAnthropic || got.Family != FamilyAnthropic {
		t.Errorf("same-family sum must keep provider/family: %+v", got)
	}
}

func TestSumCostShares_ThinkingAndMixedFamilies(t *testing.T) {
	t.Parallel()

	a := CostShares{Provider: ProviderAnthropic, Family: FamilyAnthropic, Units: 1000, Output: 1, Thinking: 0.5}
	b := CostShares{Provider: ProviderOpenAI, Family: FamilyOpenAI6x, Units: 1000, Output: 1, Thinking: 0.1}
	got := SumCostShares(a, b)
	assertClose(t, got.Thinking, 600.0/2000)
	if got.Family != "" || got.Provider != "" {
		t.Errorf("mixed families must clear family and provider: %+v", got)
	}

	// Same provider, different family: provider kept, family cleared.
	c := CostShares{Provider: ProviderOpenAI, Family: FamilyOpenAI8x, Units: 1000, Output: 1}
	got = SumCostShares(b, c)
	if got.Provider != ProviderOpenAI || got.Family != "" {
		t.Errorf("same provider, mixed family: %+v", got)
	}
}

func TestSumCostShares_EmptyProviderFamilyDoesNotVote(t *testing.T) {
	t.Parallel()

	a := CostShares{Provider: ProviderAnthropic, Family: FamilyAnthropic, Units: 1000, Output: 1}
	b := CostShares{Provider: ProviderAnthropic, Family: FamilyAnthropic, Units: 500, CacheRead: 1}
	// An unknown model prices to zero Weights: Units 0 and no provider/family.
	unknown := ComputeCostShares(&types.TokenUsage{OutputTokens: 100}, Weights{})
	if unknown.Units != 0 || unknown.Provider != "" || unknown.Family != "" {
		t.Fatalf("unknown-model shares: %+v", unknown)
	}

	// Empty parts carry no information, in any position.
	for name, parts := range map[string][]CostShares{
		"bare zero first":  {{}, a, b},
		"bare zero middle": {a, {}, b},
		"unknown last":     {a, b, unknown},
		"unknown first":    {unknown, a, b},
	} {
		got := SumCostShares(parts...)
		if got.Provider != ProviderAnthropic || got.Family != FamilyAnthropic {
			t.Errorf("%s: provider/family %q/%q, want anthropic/anthropic", name, got.Provider, got.Family)
		}
		assertCloseRel(t, got.Units, 1500)
		assertClose(t, got.Output, 1000.0/1500)
	}

	// Only named parts vote: a real disagreement still clears, wherever the
	// empty part sits.
	openAI := CostShares{Provider: ProviderOpenAI, Family: FamilyOpenAI6x, Units: 1000, Output: 1}
	got := SumCostShares(a, CostShares{}, openAI)
	if got.Provider != "" || got.Family != "" {
		t.Errorf("disagreement with an empty part in between: %+v", got)
	}

	// Parts that name nothing at all sum to nothing named.
	got = SumCostShares(CostShares{}, unknown)
	if got.Provider != "" || got.Family != "" {
		t.Errorf("all-empty parts: %+v", got)
	}
}

func TestSumCostShares_ZeroUnits(t *testing.T) {
	t.Parallel()

	for name, parts := range map[string][]CostShares{
		"no parts":    nil,
		"zero parts":  {{}, {Family: FamilyAnthropic, Provider: ProviderAnthropic}},
		"unpriced":    {{CacheWriteUnpriced: true}},
		"only zeroes": {{Units: 0, Output: 1}},
	} {
		got := SumCostShares(parts...)
		if got.Units != 0 {
			t.Errorf("%s: units %v", name, got.Units)
		}
		for label, v := range map[string]float64{"input": got.Input, "cw": got.CacheWrite, "cr": got.CacheRead, "out": got.Output, "thinking": got.Thinking} {
			if v != 0 {
				t.Errorf("%s: %s share %v, want 0", name, label, v)
			}
		}
	}
	if got := SumCostShares(CostShares{CacheWriteUnpriced: true}); !got.CacheWriteUnpriced {
		t.Error("unpriced flag must propagate even with zero units")
	}
}

func TestSumCostShares_RoundTripsComputeCostShares(t *testing.T) {
	t.Parallel()

	// Summing per-call shares must equal computing shares over the summed usage.
	w, _, _ := WeightsFor("claude-fable-5")
	calls := []*types.TokenUsage{
		{InputTokens: 100, CacheCreationTokens: 2000, CacheCreation1hTokens: 2000, CacheReadTokens: 50_000, OutputTokens: 900, ThinkingTokens: 300},
		{InputTokens: 40, CacheCreationTokens: 500, CacheCreation1hTokens: 500, CacheReadTokens: 80_000, OutputTokens: 1200, ThinkingTokens: 700},
	}
	var total *types.TokenUsage
	var parts []CostShares
	for _, c := range calls {
		total = types.AddTokenUsage(total, c)
		parts = append(parts, ComputeCostShares(c, w))
	}
	want := ComputeCostShares(total, w)
	got := SumCostShares(parts...)
	assertCloseRel(t, got.Units, want.Units)
	assertClose(t, got.Input, want.Input)
	assertClose(t, got.CacheWrite, want.CacheWrite)
	assertClose(t, got.CacheRead, want.CacheRead)
	assertClose(t, got.Output, want.Output)
	assertClose(t, got.Thinking, want.Thinking)
	if got.CacheWriteUnpriced != want.CacheWriteUnpriced || got.Family != want.Family || got.Provider != want.Provider {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
