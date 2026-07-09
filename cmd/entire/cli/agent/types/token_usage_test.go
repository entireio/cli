package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func costPtr(v float64) *float64 { return &v }

func TestAddCostUSD_NilNil(t *testing.T) {
	t.Parallel()
	if got := AddCostUSD(nil, nil); got != nil {
		t.Fatalf("AddCostUSD(nil, nil) = %v, want nil", got)
	}
}

func TestAddCostUSD_NilVal(t *testing.T) {
	t.Parallel()
	b := costPtr(1.25)
	got := AddCostUSD(nil, b)
	if got == nil || *got != 1.25 {
		t.Fatalf("AddCostUSD(nil, 1.25) = %v, want 1.25", got)
	}
	// Must be a copy, not an alias of the input.
	if got == b {
		t.Fatal("AddCostUSD(nil, b) aliased the input pointer")
	}
	*got = 99
	if *b != 1.25 {
		t.Fatalf("mutating result mutated input b: b=%v", *b)
	}

	// Symmetric: value on the a side.
	a := costPtr(2.5)
	got = AddCostUSD(a, nil)
	if got == nil || *got != 2.5 {
		t.Fatalf("AddCostUSD(2.5, nil) = %v, want 2.5", got)
	}
	if got == a {
		t.Fatal("AddCostUSD(a, nil) aliased the input pointer")
	}
}

func TestAddCostUSD_ValVal(t *testing.T) {
	t.Parallel()
	a := costPtr(1.5)
	b := costPtr(2.25)
	got := AddCostUSD(a, b)
	if got == nil || *got != 3.75 {
		t.Fatalf("AddCostUSD(1.5, 2.25) = %v, want 3.75", got)
	}
	if got == a || got == b {
		t.Fatal("AddCostUSD returned an aliased input pointer")
	}
	// Mutating result must not touch inputs.
	*got = 0
	if *a != 1.5 || *b != 2.25 {
		t.Fatalf("inputs mutated: a=%v b=%v", *a, *b)
	}
}

func TestMergeCostSource_Matrix(t *testing.T) {
	t.Parallel()
	nz := costPtr(1) // any non-nil cost
	cases := []struct {
		name         string
		a, b         string
		aCost, bCost *float64
		want         string
	}{
		{"both empty sources", "", "", nz, nz, ""},
		{"both nil cost non-empty source", CostSourceReported, CostSourceEstimated, nil, nil, ""},
		{"reported+reported", CostSourceReported, CostSourceReported, nz, nz, CostSourceReported},
		{"reported+estimated -> mixed", CostSourceReported, CostSourceEstimated, nz, nz, CostSourceMixed},
		{"estimated + empty(nil cost) -> estimated", CostSourceEstimated, "", nz, nil, CostSourceEstimated},
		{"empty(nil cost) + reported -> reported", "", CostSourceReported, nil, nz, CostSourceReported},
		{"reported + estimated(nil cost) ignores b -> reported", CostSourceReported, CostSourceEstimated, nz, nil, CostSourceReported},
		{"estimated(nil cost) + reported ignores a -> reported", CostSourceEstimated, CostSourceReported, nil, nz, CostSourceReported},
		{"mixed + reported -> mixed", CostSourceMixed, CostSourceReported, nz, nz, CostSourceMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MergeCostSource(tc.a, tc.b, tc.aCost, tc.bCost); got != tc.want {
				t.Fatalf("MergeCostSource(%q,%q,%v,%v) = %q, want %q",
					tc.a, tc.b, tc.aCost, tc.bCost, got, tc.want)
			}
		})
	}
}

func TestMergeCostSourceUsages_Matrix(t *testing.T) {
	t.Parallel()
	priced := func(source string) *TokenUsage {
		return &TokenUsage{InputTokens: 100, CostUSD: costPtr(0.5), CostSource: source}
	}
	tokensNoCost := &TokenUsage{InputTokens: 200}                   // token-bearing, unpriced
	cacheTokensNoCost := &TokenUsage{CacheReadTokens: 200}          // token-bearing (cache), unpriced
	emptyNoCost := &TokenUsage{}                                    // no tokens, no cost
	apiOnlyNoCost := &TokenUsage{APICallCount: 3}                   // api count is not billable tokens
	estimatedNoCost := &TokenUsage{CostSource: CostSourceEstimated} // label but nil cost, no tokens

	cases := []struct {
		name string
		a, b *TokenUsage
		want string
	}{
		{"priced + unpriced-with-tokens -> mixed", priced(CostSourceEstimated), tokensNoCost, CostSourceMixed},
		{"unpriced-with-tokens + priced -> mixed (order)", tokensNoCost, priced(CostSourceEstimated), CostSourceMixed},
		{"priced + unpriced-with-cache-tokens -> mixed", priced(CostSourceReported), cacheTokensNoCost, CostSourceMixed},
		{"priced + empty usage -> estimated", priced(CostSourceEstimated), emptyNoCost, CostSourceEstimated},
		{"priced + api-only usage -> reported (api not billable)", priced(CostSourceReported), apiOnlyNoCost, CostSourceReported},
		{"priced + nil -> that source", priced(CostSourceEstimated), nil, CostSourceEstimated},
		{"reported + estimated (both priced) -> mixed", priced(CostSourceReported), priced(CostSourceEstimated), CostSourceMixed},
		{"reported + reported (both priced) -> reported", priced(CostSourceReported), priced(CostSourceReported), CostSourceReported},
		{"empty + empty -> empty", emptyNoCost, emptyNoCost, ""},
		{"tokens-no-cost + estimated-label-no-cost -> empty", tokensNoCost, estimatedNoCost, ""},
		{"both nil -> empty", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MergeCostSourceUsages(tc.a, tc.b); got != tc.want {
				t.Fatalf("MergeCostSourceUsages = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTokenUsage_JSON_NilCostOmitted(t *testing.T) {
	t.Parallel()
	u := TokenUsage{InputTokens: 10, OutputTokens: 5}
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "cost_usd") {
		t.Fatalf("expected no cost_usd key for nil cost, got %s", s)
	}
	if strings.Contains(s, "cost_source") {
		t.Fatalf("expected no cost_source key for empty source, got %s", s)
	}
}

func TestTokenUsage_JSON_RoundTripWithCost(t *testing.T) {
	t.Parallel()
	u := TokenUsage{
		InputTokens:  10,
		OutputTokens: 5,
		CostUSD:      costPtr(0.42),
		CostSource:   CostSourceReported,
	}
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "cost_usd") {
		t.Fatalf("expected cost_usd key, got %s", data)
	}
	var got TokenUsage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.42 {
		t.Fatalf("CostUSD round-trip failed: %v", got.CostUSD)
	}
	if got.CostSource != CostSourceReported {
		t.Fatalf("CostSource round-trip failed: %q", got.CostSource)
	}
}

func TestTokenUsage_JSON_LegacyWithoutCostFields(t *testing.T) {
	t.Parallel()
	legacy := `{"input_tokens":10,"cache_creation_tokens":0,"cache_read_tokens":0,"output_tokens":5,"api_call_count":1}`
	var got TokenUsage
	if err := json.Unmarshal([]byte(legacy), &got); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if got.CostUSD != nil {
		t.Fatalf("legacy CostUSD = %v, want nil", got.CostUSD)
	}
	if got.CostSource != "" {
		t.Fatalf("legacy CostSource = %q, want empty", got.CostSource)
	}
}
