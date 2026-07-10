package agent

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyTierVariant covers the bucket retargeting that keeps a
// tier-suffixed fallback model effective when an agent's
// ModelUsageCalculator produces buckets under raw transcript ids.
func TestApplyTierVariant(t *testing.T) {
	t.Parallel()

	mk := func(models ...string) []types.ModelUsage {
		buckets := make([]types.ModelUsage, 0, len(models))
		for _, m := range models {
			buckets = append(buckets, types.ModelUsage{Model: m})
		}
		return buckets
	}
	models := func(buckets []types.ModelUsage) []string {
		out := make([]string, 0, len(buckets))
		for _, b := range buckets {
			out = append(out, b.Model)
		}
		return out
	}

	tests := []struct {
		name     string
		buckets  []types.ModelUsage
		fallback string
		want     []string
	}{
		{
			name:     "matching base is retargeted onto the variant",
			buckets:  mk("gpt-5.5"),
			fallback: "gpt-5.5-priority",
			want:     []string{"gpt-5.5-priority"},
		},
		{
			name:     "only the matching bucket moves in a mixed session",
			buckets:  mk("gpt-5.5", "gpt-5.4", "", "gpt-5.5-priority"),
			fallback: "gpt-5.5-priority",
			want:     []string{"gpt-5.5-priority", "gpt-5.4", "", "gpt-5.5-priority"},
		},
		{
			name:     "unsuffixed fallback is a no-op",
			buckets:  mk("gpt-5.5", ""),
			fallback: "gpt-5.5",
			want:     []string{"gpt-5.5", ""},
		},
		{
			name:     "empty fallback is a no-op",
			buckets:  mk("gpt-5.5"),
			fallback: "",
			want:     []string{"gpt-5.5"},
		},
		{
			name:     "bare suffix has no base and is a no-op",
			buckets:  mk("gpt-5.5", ""),
			fallback: "-priority",
			want:     []string{"gpt-5.5", ""},
		},
		{
			name:     "non-matching base leaves everything raw",
			buckets:  mk("claude-opus-4-8"),
			fallback: "gpt-5.5-priority",
			want:     []string{"claude-opus-4-8"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			applyTierVariant(tt.buckets, tt.fallback)
			assert.Equal(t, tt.want, models(tt.buckets))
		})
	}
}

// TestResolveTierFallback covers the downgrade of a tier-suffixed fallback
// model to its base id when the pricing table has no entry for the variant, so
// the session prices as an honest standard-rate undercount instead of going
// entirely unpriced under a premium-claiming id.
func TestResolveTierFallback(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	require.NoError(t, err)

	tests := []struct {
		name     string
		fallback string
		table    *pricing.Table
		want     string
	}{
		{
			name:     "priceable variant is kept",
			fallback: "gpt-5.5-priority",
			table:    table,
			want:     "gpt-5.5-priority",
		},
		{
			name:     "unpriceable variant downgrades to its base",
			fallback: "gpt-5.3-codex-priority",
			table:    table,
			want:     "gpt-5.3-codex",
		},
		{
			name:     "unsuffixed model passes through",
			fallback: "gpt-5.3-codex",
			table:    table,
			want:     "gpt-5.3-codex",
		},
		{
			name:     "nil table keeps the base id",
			fallback: "gpt-5.5-priority",
			table:    nil,
			want:     "gpt-5.5",
		},
		{
			name:     "bare suffix passes through",
			fallback: "-priority",
			table:    table,
			want:     "-priority",
		},
		{
			name:     "empty model passes through",
			fallback: "",
			table:    table,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resolveTierFallback(tt.fallback, tt.table))
		})
	}
}
