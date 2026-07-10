package pricing

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGoogleGemini31And35 covers the gemini-3.1-pro and gemini-3.5-flash entries
// added because Cursor CLI and Gemini CLI report these ids today, while the table
// previously carried only the 3.0 generation.
func TestGoogleGemini31And35(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	proSample := types.TokenUsage{InputTokens: 50_000, OutputTokens: 20_000, CacheReadTokens: 8_000, CacheCreationTokens: 4_000}

	// gemini-3.1-pro: $2/$12, cache read $0.20.
	pro, ok := tbl.Lookup("gemini-3.1-pro")
	require.True(t, ok, "gemini-3.1-pro must resolve")
	assert.Equal(t, "gemini-3.1-pro", pro.ID)
	assert.InDelta(t, 2.0, pro.InputPerMTok, 1e-9)
	assert.InDelta(t, 12.0, pro.OutputPerMTok, 1e-9)
	require.NotNil(t, pro.CacheReadPerMTok)
	assert.InDelta(t, 0.2, *pro.CacheReadPerMTok, 1e-9)
	// 50000*2 + 20000*12 + 8000*0.2 + 4000*2 = 349600 -> $0.3496.
	assert.InDelta(t, 0.3496, Estimate(pro, proSample), 1e-9)

	// gemini-3.5-flash: $1.50/$9, cache read $0.15.
	flash, ok := tbl.Lookup("gemini-3.5-flash")
	require.True(t, ok, "gemini-3.5-flash must resolve")
	assert.Equal(t, "gemini-3.5-flash", flash.ID)
	assert.InDelta(t, 1.5, flash.InputPerMTok, 1e-9)
	assert.InDelta(t, 9.0, flash.OutputPerMTok, 1e-9)
	require.NotNil(t, flash.CacheReadPerMTok)
	assert.InDelta(t, 0.15, *flash.CacheReadPerMTok, 1e-9)
	// 50000*1.5 + 20000*9 + 8000*0.15 + 4000*1.5 = 262200 -> $0.2622.
	assert.InDelta(t, 0.2622, Estimate(flash, proSample), 1e-9)

	// Alias forms resolve.
	for q, wantID := range map[string]string{
		"google/gemini-3.1-pro":    "gemini-3.1-pro",
		"gemini-3.1-pro-preview":   "gemini-3.1-pro",
		"google/gemini-3.5-flash":  "gemini-3.5-flash",
		"gemini-3.5-flash-preview": "gemini-3.5-flash",
	} {
		got, ok := tbl.Lookup(q)
		require.Truef(t, ok, "%q must resolve", q)
		assert.Equalf(t, wantID, got.ID, "Lookup(%q)", q)
	}
}

// TestGoogleGeminiDottedGlobsDoNotCrossMatch guards the boundary between the
// dotted 3.1/3.5 ids and the 3.0 globs: path.Match's "3.1"/"3.5" must not fall
// into the "gemini-3-pro-*" / "gemini-3-flash-*" entries, and the reverse.
func TestGoogleGeminiDottedGlobsDoNotCrossMatch(t *testing.T) {
	t.Parallel()

	tbl, err := LoadTable(nil)
	require.NoError(t, err)

	cases := []struct {
		query  string
		wantID string
	}{
		// 3.0 dated variants stay on the 3.0 entries (new dotted globs don't swallow them).
		{"gemini-3-pro-preview", "gemini-3-pro"},
		{"gemini-3-flash-preview", "gemini-3-flash"},
		// dotted dated variants stay on the dotted entries (3.0 globs don't swallow them).
		{"gemini-3.1-pro-preview", "gemini-3.1-pro"},
		{"gemini-3.5-flash-preview", "gemini-3.5-flash"},
		// exact base ids unaffected.
		{"gemini-3-pro", "gemini-3-pro"},
		{"gemini-3-flash", "gemini-3-flash"},
	}
	for _, tc := range cases {
		got, ok := tbl.Lookup(tc.query)
		require.Truef(t, ok, "%q must resolve", tc.query)
		assert.Equalf(t, tc.wantID, got.ID, "Lookup(%q)", tc.query)
	}
}
