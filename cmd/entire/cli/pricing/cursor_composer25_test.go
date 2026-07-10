package pricing

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCursorComposer25 covers the composer-2.5 standard entry added because real
// Cursor CLI sessions report the bare id "composer-2.5" (the current default),
// which previously resolved to no rate and left cost nil.
func TestCursorComposer25(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	// Exact id resolves to the standard rate (same $0.50/$2.50 as composer-2).
	rate, ok := tbl.Lookup("composer-2.5")
	require.True(t, ok, "composer-2.5 must resolve")
	assert.Equal(t, "composer-2.5", rate.ID)
	assert.Equal(t, "cursor", rate.Provider)
	assert.InDelta(t, 0.5, rate.InputPerMTok, 1e-9)
	assert.InDelta(t, 2.5, rate.OutputPerMTok, 1e-9)
	require.NotNil(t, rate.CacheReadPerMTok)
	assert.InDelta(t, 0.05, *rate.CacheReadPerMTok, 1e-9)
	require.NotNil(t, rate.CacheWritePerMTok)
	assert.InDelta(t, 0.5, *rate.CacheWritePerMTok, 1e-9)

	// Estimate matches hand math:
	// 100000*0.5 + 40000*2.5 + 10000*0.05 + 5000*0.5 = 153000 -> $0.153.
	usage := types.TokenUsage{
		InputTokens:         100_000,
		OutputTokens:        40_000,
		CacheReadTokens:     10_000,
		CacheCreationTokens: 5_000,
	}
	assert.InDelta(t, 0.153, Estimate(rate, usage), 1e-9)

	// Alias forms resolve to composer-2.5.
	for _, q := range []string{"cursor/composer-2.5", "composer-2.5-2026-05-18"} {
		got, ok := tbl.Lookup(q)
		require.Truef(t, ok, "%q must resolve", q)
		assert.Equalf(t, "composer-2.5", got.ID, "%q should resolve to composer-2.5", q)
	}
}

// TestCursorComposer25NoCrossMatch guards the exact-match-wins boundary between
// composer-2.5 and the pre-existing composer-2.5-fast and composer-2 entries: a
// glob from one family must never swallow the other's ids.
func TestCursorComposer25NoCrossMatch(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	cases := []struct {
		query  string
		wantID string
	}{
		{"composer-2.5", "composer-2.5"},                      // exact standard
		{"composer-2.5-fast", "composer-2.5-fast"},            // exact fast wins over composer-2.5 glob
		{"composer-2.5-2026-05-18", "composer-2.5"},           // dated standard -> standard
		{"composer-2.5-fast-2026-05-18", "composer-2.5-fast"}, // dated fast -> fast
		{"composer-2", "composer-2"},                          // unrelated base id unaffected
	}
	for _, tc := range cases {
		got, ok := tbl.Lookup(tc.query)
		require.Truef(t, ok, "%q must resolve", tc.query)
		assert.Equalf(t, tc.wantID, got.ID, "Lookup(%q)", tc.query)
	}

	// composer-2.5 and composer-2.5-fast are distinct entries with distinct rates:
	// standard $2.50 output vs fast $15.00 output.
	std, _ := tbl.Lookup("composer-2.5")
	fast, _ := tbl.Lookup("composer-2.5-fast")
	assert.InDelta(t, 2.5, std.OutputPerMTok, 1e-9)
	assert.InDelta(t, 15.0, fast.OutputPerMTok, 1e-9)
}
