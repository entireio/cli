package pricing

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modelOpus48 = "claude-opus-4-8"
	modelGPT55  = "gpt-5.5"
)

func floatPtr(f float64) *float64 { return &f }

func TestLoadTable_EmbeddedParsesAndValidates(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)
	require.NotNil(t, tbl)

	// A representative model from each embedded provider file resolves.
	for _, id := range []string{modelOpus48, modelGPT55, "gemini-3-pro"} {
		_, ok := tbl.Lookup(id)
		assert.Truef(t, ok, "expected embedded model %q to resolve", id)
	}
}

func TestTable_Lookup(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	tests := []struct {
		name   string
		query  string
		wantID string
		wantOK bool
	}{
		{name: "exact id", query: modelOpus48, wantID: modelOpus48, wantOK: true},
		{name: "alias glob dated", query: "claude-opus-4-8-20260624", wantID: modelOpus48, wantOK: true},
		{name: "alias provider-prefixed", query: "anthropic/claude-opus-4-8", wantID: modelOpus48, wantOK: true},
		{name: "case insensitive id", query: "CLAUDE-OPUS-4-8", wantID: modelOpus48, wantOK: true},
		{name: "case insensitive with whitespace", query: "  Claude-Opus-4-8  ", wantID: modelOpus48, wantOK: true},
		{name: "openai exact", query: modelGPT55, wantID: modelGPT55, wantOK: true},
		{name: "openai alias glob", query: "gpt-5.5-2026-01-01", wantID: modelGPT55, wantOK: true},
		{name: "miss returns false", query: "totally-unknown-model", wantOK: false},
		{name: "empty returns false", query: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tbl.Lookup(tc.query)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantID, got.ID)
			}
		})
	}
}

func TestTable_Lookup_AnthropicIDForms(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	tests := []struct {
		name   string
		query  string
		wantID string
		wantOK bool
	}{
		{name: "bare id", query: "claude-opus-4-8", wantID: "claude-opus-4-8", wantOK: true},
		{name: "anthropic dated", query: "claude-haiku-4-5-20251001", wantID: "claude-haiku-4-5", wantOK: true},
		{name: "long-context 1m suffix on bare id", query: "claude-fable-5[1m]", wantID: "claude-fable-5", wantOK: true},
		{name: "long-context 1m suffix on dated id", query: "claude-opus-4-8-20260624[1m]", wantID: "claude-opus-4-8", wantOK: true},
		{name: "long-context 1m suffix uppercase", query: "CLAUDE-SONNET-5[1M]", wantID: "claude-sonnet-5", wantOK: true},
		{name: "bedrock bare", query: "anthropic.claude-opus-4-8", wantID: "claude-opus-4-8", wantOK: true},
		{name: "bedrock versioned regional", query: "us.anthropic.claude-opus-4-8-v1:0", wantID: "claude-opus-4-8", wantOK: true},
		{name: "bedrock legacy dated versioned", query: "anthropic.claude-3-5-sonnet-20241022-v2:0", wantID: "claude-3-5-sonnet", wantOK: true},
		{name: "vertex dated", query: "claude-opus-4-5@20251101", wantID: "claude-opus-4-5", wantOK: true},
		{name: "slash prefixed", query: "anthropic/claude-sonnet-5", wantID: "claude-sonnet-5", wantOK: true},
		{name: "legacy dated sonnet", query: "claude-3-5-sonnet-20241022", wantID: "claude-3-5-sonnet", wantOK: true},
		{name: "legacy opus canonical alias", query: "claude-3-opus", wantID: "claude-opus-3", wantOK: true},
		{name: "legacy opus dated alias", query: "claude-3-opus-20240229", wantID: "claude-opus-3", wantOK: true},
		{name: "legacy haiku dated", query: "claude-3-haiku-20240307", wantID: "claude-3-haiku", wantOK: true},
		{name: "non-claude id misses", query: "meta-llama-3-70b", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tbl.Lookup(tc.query)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantID, got.ID)
			}
		})
	}
}

func TestTable_Lookup_LongContextSharesBaseRate(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	// The "[1m]" long-context id resolves to the base model and bills at the
	// base rate (no long-context premium).
	got, ok := tbl.Lookup("claude-fable-5[1m]")
	require.True(t, ok)
	assert.Equal(t, "claude-fable-5", got.ID)
	assert.InDelta(t, 10.0, got.InputPerMTok, 1e-9)
	assert.InDelta(t, 50.0, got.OutputPerMTok, 1e-9)
}

func TestEstimate_DefaultMultipliers(t *testing.T) {
	t.Parallel()

	// claude-opus-4-8: $5 input / $25 output per MTok, no explicit cache rates,
	// so cache-read defaults to 0.1x and cache-write to 1.25x of input.
	r := ModelRate{
		ID:            modelOpus48,
		Provider:      "anthropic",
		InputPerMTok:  5,
		OutputPerMTok: 25,
	}
	u := types.TokenUsage{
		InputTokens:         1_000_000, // 1M fresh input   -> 5 * 1   = 5.00
		CacheReadTokens:     1_000_000, // 1M cache read    -> 0.5 * 1  = 0.50
		CacheCreationTokens: 1_000_000, // 1M cache write   -> 6.25 * 1 = 6.25
		OutputTokens:        1_000_000, // 1M output        -> 25 * 1   = 25.00
	}

	assert.InDelta(t, 36.75, Estimate(r, u), 1e-9)
}

func TestEstimate_ExplicitCacheRatesOverrideMultipliers(t *testing.T) {
	t.Parallel()

	// Explicit cache rates must be used verbatim instead of the multipliers.
	r := ModelRate{
		ID:                modelGPT55,
		Provider:          "openai",
		InputPerMTok:      10,
		OutputPerMTok:     10,
		CacheReadPerMTok:  floatPtr(3),
		CacheWritePerMTok: floatPtr(2),
	}
	u := types.TokenUsage{
		InputTokens:         1_000_000, // 10 * 1 = 10
		CacheReadTokens:     1_000_000, // explicit 3 * 1 = 3  (multiplier would give 0.1*10 = 1)
		CacheCreationTokens: 1_000_000, // explicit 2 * 1 = 2  (multiplier would give 1.25*10 = 12.5)
		OutputTokens:        1_000_000, // 10 * 1 = 10
	}

	// With multipliers this would be 10 + 1 + 12.5 + 10 = 33.5; explicit rates give 25.
	assert.InDelta(t, 25.0, Estimate(r, u), 1e-9)
}

func TestLoadTable_OverrideReplacesByID(t *testing.T) {
	t.Parallel()

	override := ModelRate{
		ID:            modelOpus48,
		Provider:      "anthropic",
		InputPerMTok:  99,
		OutputPerMTok: 199,
	}

	tbl, err := LoadTable([]ModelRate{override})
	require.NoError(t, err)

	got, ok := tbl.Lookup(modelOpus48)
	require.True(t, ok)
	assert.InDelta(t, 99.0, got.InputPerMTok, 1e-9)
	assert.InDelta(t, 199.0, got.OutputPerMTok, 1e-9)

	// Replacement must not duplicate the id in the table.
	base, err := LoadTable(nil)
	require.NoError(t, err)
	assert.Len(t, tbl.models, len(base.models))
}

func TestLoadTable_OverrideAppendsNewID(t *testing.T) {
	t.Parallel()

	override := ModelRate{
		ID:            "custom-inhouse-model",
		Provider:      "inhouse",
		InputPerMTok:  7,
		OutputPerMTok: 8,
	}

	base, err := LoadTable(nil)
	require.NoError(t, err)

	tbl, err := LoadTable([]ModelRate{override})
	require.NoError(t, err)

	got, ok := tbl.Lookup("custom-inhouse-model")
	require.True(t, ok)
	assert.InDelta(t, 7.0, got.InputPerMTok, 1e-9)
	assert.Len(t, tbl.models, len(base.models)+1)
}

func TestLoadTable_RejectsInvalidOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override ModelRate
	}{
		{name: "empty id", override: ModelRate{InputPerMTok: 1, OutputPerMTok: 2}},
		{name: "non-positive input", override: ModelRate{ID: "x", InputPerMTok: 0, OutputPerMTok: 2}},
		{name: "non-positive output", override: ModelRate{ID: "x", InputPerMTok: 1, OutputPerMTok: 0}},
		{name: "negative cache read", override: ModelRate{ID: "x", InputPerMTok: 1, OutputPerMTok: 2, CacheReadPerMTok: floatPtr(-1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadTable([]ModelRate{tc.override})
			require.Error(t, err)
		})
	}
}
