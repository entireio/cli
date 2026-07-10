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
	for _, id := range []string{modelOpus48, modelGPT55, "gemini-3-pro", "composer-2"} {
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
		{name: "bedrock global inference profile", query: "global.anthropic.claude-opus-4-8-v1:0", wantID: "claude-opus-4-8", wantOK: true},
		{name: "vertex dated", query: "claude-opus-4-5@20251101", wantID: "claude-opus-4-5", wantOK: true},
		{name: "vertex legacy versioned", query: "claude-3-5-sonnet-v2@20241022", wantID: "claude-3-5-sonnet", wantOK: true},
		{name: "slash prefixed", query: "anthropic/claude-sonnet-5", wantID: "claude-sonnet-5", wantOK: true},
		{name: "slash prefixed dated", query: "anthropic/claude-3-5-sonnet-20241022", wantID: "claude-3-5-sonnet", wantOK: true},
		{name: "legacy dated sonnet", query: "claude-3-5-sonnet-20241022", wantID: "claude-3-5-sonnet", wantOK: true},
		{name: "legacy opus canonical alias", query: "claude-3-opus", wantID: "claude-opus-3", wantOK: true},
		{name: "legacy opus dated alias", query: "claude-3-opus-20240229", wantID: "claude-opus-3", wantOK: true},
		{name: "legacy haiku dated", query: "claude-3-haiku-20240307", wantID: "claude-3-haiku", wantOK: true},
		{name: "gpt-5.6 sol resolves", query: "gpt-5.6-sol", wantID: "gpt-5.6-sol", wantOK: true},
		{name: "gpt-5.6 sol dated resolves", query: "gpt-5.6-sol-20260709", wantID: "gpt-5.6-sol", wantOK: true},
		{name: "gpt-5.6-sol-pro must NOT resolve to sol", query: "gpt-5.6-sol-pro", wantOK: false},
		{name: "prefixed gpt-5.6-sol-pro must NOT resolve to sol", query: "openai/gpt-5.6-sol-pro", wantOK: false},
		{name: "global-prefixed gpt-5.6-sol-pro must NOT resolve to sol", query: "global.openai.gpt-5.6-sol-pro", wantOK: false},
		{name: "prefixed gpt-5.6-sol exact resolves", query: "openai/gpt-5.6-sol", wantID: "gpt-5.6-sol", wantOK: true},
		{name: "prefixed gpt-5.6-sol dated resolves", query: "openai/gpt-5.6-sol-20260709", wantID: "gpt-5.6-sol", wantOK: true},
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

func TestTable_Round2CorrectedAndNewRates(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	// Corrected OpenAI rates: the table previously carried the legacy gpt-5 rate
	// (1.25/10) for both gpt-5.5 and gpt-5.4.
	gpt55, ok := tbl.Lookup("gpt-5.5")
	require.True(t, ok)
	assert.InDelta(t, 5.0, gpt55.InputPerMTok, 1e-9)
	assert.InDelta(t, 30.0, gpt55.OutputPerMTok, 1e-9)

	gpt54, ok := tbl.Lookup("gpt-5.4")
	require.True(t, ok)
	assert.InDelta(t, 2.5, gpt54.InputPerMTok, 1e-9)
	assert.InDelta(t, 15.0, gpt54.OutputPerMTok, 1e-9)

	// gpt-5 keeps its genuine legacy rate.
	gpt5, ok := tbl.Lookup("gpt-5")
	require.True(t, ok)
	assert.InDelta(t, 1.25, gpt5.InputPerMTok, 1e-9)
	assert.InDelta(t, 10.0, gpt5.OutputPerMTok, 1e-9)

	// New entries: gpt-5.3-codex, gpt-5.5-priority, and the cursor composer family.
	codex, ok := tbl.Lookup("gpt-5.3-codex")
	require.True(t, ok)
	assert.InDelta(t, 1.75, codex.InputPerMTok, 1e-9)
	assert.InDelta(t, 14.0, codex.OutputPerMTok, 1e-9)

	prio, ok := tbl.Lookup("gpt-5.5-priority")
	require.True(t, ok)
	assert.InDelta(t, 12.5, prio.InputPerMTok, 1e-9)
	assert.InDelta(t, 75.0, prio.OutputPerMTok, 1e-9)

	composer, ok := tbl.Lookup("composer-2.5-fast")
	require.True(t, ok)
	assert.InDelta(t, 3.0, composer.InputPerMTok, 1e-9)
	assert.InDelta(t, 15.0, composer.OutputPerMTok, 1e-9)
}

func TestTable_GPT55PriorityDistinctFromBase(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	// Both are exact ids resolving to their own entries: the base gpt-5.5 glob
	// must not swallow the priority id, nor vice versa.
	base, ok := tbl.Lookup("gpt-5.5")
	require.True(t, ok)
	assert.Equal(t, "gpt-5.5", base.ID)
	assert.InDelta(t, 5.0, base.InputPerMTok, 1e-9)

	prio, ok := tbl.Lookup("gpt-5.5-priority")
	require.True(t, ok)
	assert.Equal(t, "gpt-5.5-priority", prio.ID)
	assert.InDelta(t, 12.5, prio.InputPerMTok, 1e-9)

	// A dated priority spelling resolves to the priority entry — it is not
	// glob-captured by the base gpt-5.5 alias (tightened to "gpt-5.5-2*").
	datedPrio, ok := tbl.Lookup("gpt-5.5-priority-20260710")
	require.True(t, ok)
	assert.Equal(t, "gpt-5.5-priority", datedPrio.ID)

	// A dated base spelling still resolves to the base entry.
	datedBase, ok := tbl.Lookup("gpt-5.5-20260710")
	require.True(t, ok)
	assert.Equal(t, "gpt-5.5", datedBase.ID)
}

func TestTable_OpusFastVariant(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	fast, ok := tbl.Lookup("claude-opus-4-8-fast")
	require.True(t, ok)
	assert.Equal(t, "claude-opus-4-8-fast", fast.ID)
	assert.InDelta(t, 10.0, fast.InputPerMTok, 1e-9)
	assert.InDelta(t, 50.0, fast.OutputPerMTok, 1e-9)

	// The fast entry carries no explicit cache rates, so Estimate derives them
	// from the fast input rate via the Anthropic multipliers: cache-read 0.1x
	// ($1.00 per MTok) and cache-write 1.25x ($12.50 per MTok).
	require.Nil(t, fast.CacheReadPerMTok)
	require.Nil(t, fast.CacheWritePerMTok)
	assert.InDelta(t, 1.0, Estimate(fast, types.TokenUsage{CacheReadTokens: 1_000_000}), 1e-9)
	assert.InDelta(t, 12.5, Estimate(fast, types.TokenUsage{CacheCreationTokens: 1_000_000}), 1e-9)

	// The dated fast spelling resolves to the fast entry, not the base
	// claude-opus-4-8 (whose "claude-opus-4-8-2*" glob would otherwise capture
	// it — the fast entry is ordered first so its "-fast" glob wins).
	dated, ok := tbl.Lookup("claude-opus-4-8-20260624-fast")
	require.True(t, ok)
	assert.Equal(t, "claude-opus-4-8-fast", dated.ID)

	// A dated base spelling (no -fast) still resolves to the base entry.
	datedBase, ok := tbl.Lookup("claude-opus-4-8-20260624")
	require.True(t, ok)
	assert.Equal(t, "claude-opus-4-8", datedBase.ID)

	// The OpenRouter dotted fast spelling resolves too.
	dotted, ok := tbl.Lookup("anthropic/claude-opus-4.8-fast")
	require.True(t, ok)
	assert.Equal(t, "claude-opus-4-8-fast", dotted.ID)
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

	// Deliberately minimal: no provider, no aliases, no cache rates — the
	// natural shape of a price-bump override. Everything else must inherit.
	override := ModelRate{
		ID:            modelOpus48,
		InputPerMTok:  99,
		OutputPerMTok: 199,
	}

	tbl, err := LoadTable([]ModelRate{override})
	require.NoError(t, err)

	got, ok := tbl.Lookup(modelOpus48)
	require.True(t, ok)
	assert.InDelta(t, 99.0, got.InputPerMTok, 1e-9)
	assert.InDelta(t, 199.0, got.OutputPerMTok, 1e-9)

	// The override carried no aliases, so it must inherit the embedded entry's
	// aliases: a dated spelling still resolves, and to the overridden price.
	dated, ok := tbl.Lookup("claude-opus-4-8-20260624")
	require.True(t, ok, "dated alias must still resolve after an alias-less override")
	assert.Equal(t, modelOpus48, dated.ID)
	assert.InDelta(t, 99.0, dated.InputPerMTok, 1e-9)

	// Replacement must not duplicate the id in the table.
	base, err := LoadTable(nil)
	require.NoError(t, err)
	assert.Len(t, tbl.models, len(base.models))

	// The override omitted provider, so it must inherit the embedded entry's
	// provider and keep Anthropic cache economics: 1M cache-read tokens at the
	// overridden $99 input rate bill at the 0.1x multiplier, not full input.
	assert.Equal(t, "anthropic", got.Provider)
	cacheOnly := types.TokenUsage{CacheReadTokens: 1_000_000}
	assert.InDelta(t, 9.9, Estimate(got, cacheOnly), 1e-9)
}

func TestLoadTable_OverrideInheritsCacheRates(t *testing.T) {
	t.Parallel()

	// gpt-5.5 carries explicit cache rates in the embedded table. A minimal
	// provider-less override must inherit provider AND both cache-rate
	// pointers, so cache tokens keep billing at the explicit OpenAI rates
	// rather than falling back to full input rate.
	base, err := LoadTable(nil)
	require.NoError(t, err)
	embedded, ok := base.Lookup(modelGPT55)
	require.True(t, ok)
	require.NotNil(t, embedded.CacheReadPerMTok)

	tbl, err := LoadTable([]ModelRate{{ID: modelGPT55, InputPerMTok: 2, OutputPerMTok: 20}})
	require.NoError(t, err)
	got, ok := tbl.Lookup(modelGPT55)
	require.True(t, ok)
	assert.Equal(t, embedded.Provider, got.Provider)
	require.NotNil(t, got.CacheReadPerMTok)
	assert.InDelta(t, *embedded.CacheReadPerMTok, *got.CacheReadPerMTok, 1e-9)
	require.NotNil(t, got.CacheWritePerMTok)
	assert.InDelta(t, *embedded.CacheWritePerMTok, *got.CacheWritePerMTok, 1e-9)
}

func TestLoadTable_OverrideCanonicalizesID(t *testing.T) {
	t.Parallel()

	base, err := LoadTable(nil)
	require.NoError(t, err)

	// An override whose id differs from the builtin only in case or surrounding
	// whitespace must REPLACE the builtin, not append a phantom duplicate.
	for _, variant := range []string{"Claude-Opus-4-8", "claude-opus-4-8 ", "  CLAUDE-OPUS-4-8  "} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()

			tbl, err := LoadTable([]ModelRate{{
				ID:            variant,
				Provider:      "anthropic",
				InputPerMTok:  42,
				OutputPerMTok: 84,
			}})
			require.NoError(t, err)

			got, ok := tbl.Lookup(modelOpus48)
			require.True(t, ok)
			assert.InDelta(t, 42.0, got.InputPerMTok, 1e-9, "override must take effect")

			// No phantom entry: table size is unchanged from the embedded defaults.
			assert.Len(t, tbl.models, len(base.models))
		})
	}
}

func TestEstimate_NonAnthropicCacheFallbackBillsAsInput(t *testing.T) {
	t.Parallel()

	// A non-anthropic rate with no explicit cache rates: both cache-read and
	// cache-write fall back to the full input rate (1.0x), NOT the Anthropic
	// 0.1x / 1.25x multipliers. Provider casing is ignored.
	r := ModelRate{
		ID:            "some-openai-model",
		Provider:      "OpenAI",
		InputPerMTok:  10,
		OutputPerMTok: 20,
	}
	u := types.TokenUsage{
		InputTokens:         1_000_000, // 10 * 1 = 10
		CacheReadTokens:     1_000_000, // 1.0x input = 10 (anthropic would be 1)
		CacheCreationTokens: 1_000_000, // 1.0x input = 10 (anthropic would be 12.5)
		OutputTokens:        1_000_000, // 20 * 1 = 20
	}

	// 10 + 10 + 10 + 20 = 50 (anthropic multipliers would give 10+1+12.5+20 = 43.5).
	assert.InDelta(t, 50.0, Estimate(r, u), 1e-9)
}

func TestLoadTable_RejectsBadAliasGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		alias string
	}{
		{name: "unterminated character class", alias: "claude-[bad"},
		{name: "whitespace-only alias", alias: " "},
		{name: "empty alias", alias: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadTable([]ModelRate{{
				ID:            "custom-model",
				Provider:      "inhouse",
				Aliases:       []string{tc.alias},
				InputPerMTok:  1,
				OutputPerMTok: 2,
			}})
			require.Error(t, err)
		})
	}
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
