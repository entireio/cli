package cli

import (
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// tokenWeights are a model's per-token price ratios relative to its own base
// input price (Input is 1). They carry no currency: multiplying token counts by
// them yields dimensionless units that are comparable only as shares of a
// total. Entire never prices anyone's account.
//
// A zero CacheWrite5m means the provider does not charge for cache writes.
type tokenWeights struct {
	Family       string
	Input        float64
	CacheWrite5m float64
	CacheWrite1h float64
	CacheRead    float64
	Output       float64
}

// Price ratios per model family, verified 2026-08-28 against the providers'
// public pricing pages. Re-verify quarterly and bump the date.
//
//	Anthropic: https://www.anthropic.com/pricing
//	           https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
//	OpenAI:    https://openai.com/api/pricing/
//	Google:    https://ai.google.dev/gemini-api/docs/pricing
//
// Notes:
//   - Anthropic's 1M context is standard pricing on 4.6+, so `[1m]` models take
//     the same row with no multiplier.
//   - Gemini's cache storage is billed per hour and cannot be derived from token
//     counts, so every Gemini row has a zero cache-write ratio.
//   - Long-context tiers (OpenAI 5.4+ above 272k input, Gemini 2.5 Pro above
//     200k) are decidable only per call. This file works from a checkpoint
//     total, so it deliberately applies base-tier ratios only; a report built
//     from per-call data can refine that later.
//   - Anthropic fast mode and data-residency uplifts are not detectable from a
//     transcript and are not modelled.
var tokenFamilyWeights = map[string]tokenWeights{
	"anthropic":             {Family: "anthropic", Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, Output: 5},
	"openai-8x":             {Family: "openai-8x", Input: 1, CacheRead: 0.1, Output: 8},
	"openai-6x":             {Family: "openai-6x", Input: 1, CacheRead: 0.1, Output: 6},
	"openai-5.6-sol":        {Family: "openai-5.6-sol", Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 5},
	"openai-5.6-terra-luna": {Family: "openai-5.6-terra-luna", Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 6},
	"gemini-2.5-flash":      {Family: "gemini-2.5-flash", Input: 1, CacheRead: 0.1, Output: 8.33},
	"gemini-2.5-pro":        {Family: "gemini-2.5-pro", Input: 1, CacheRead: 0.1, Output: 8},
	"gemini-3.5-flash":      {Family: "gemini-3.5-flash", Input: 1, CacheRead: 0.1, Output: 6},
	"gemini-3.6-flash":      {Family: "gemini-3.6-flash", Input: 1, CacheRead: 0.1, Output: 5},
}

// tokenFamilyForModel maps a recorded model name to a verified ratio row.
// An unrecognised model returns ok=false: callers report volume only rather
// than pricing it against a guessed row.
func tokenFamilyForModel(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return "", false
	case strings.HasPrefix(m, "claude-"):
		return "anthropic", true
	case strings.HasPrefix(m, "gpt-5.6-sol"):
		return "openai-5.6-sol", true
	case strings.HasPrefix(m, "gpt-5.6-terra"), strings.HasPrefix(m, "gpt-5.6-luna"):
		return "openai-5.6-terra-luna", true
	case strings.HasPrefix(m, "gpt-5.4"), strings.HasPrefix(m, "gpt-5.5"):
		return "openai-6x", true
	case m == "gpt-5", strings.HasPrefix(m, "gpt-5-"), strings.HasPrefix(m, "gpt-5.1"),
		strings.HasPrefix(m, "gpt-5.2"), strings.HasPrefix(m, "gpt-5.3-codex"):
		return "openai-8x", true
	case strings.HasPrefix(m, "gemini-2.5-flash"):
		return "gemini-2.5-flash", true
	case strings.HasPrefix(m, "gemini-2.5-pro"):
		return "gemini-2.5-pro", true
	case strings.HasPrefix(m, "gemini-3.5-flash"):
		return "gemini-3.5-flash", true
	case strings.HasPrefix(m, "gemini-3.6-flash"), strings.HasPrefix(m, "gemini-3.7-flash"):
		return "gemini-3.6-flash", true
	default:
		return "", false
	}
}

// tokenWeightsForModel returns the base-tier ratios for model. ok=false means
// the model has no verified row and the report must show volume only.
func tokenWeightsForModel(model string) (tokenWeights, bool) {
	family, ok := tokenFamilyForModel(model)
	if !ok {
		return tokenWeights{}, false
	}
	return tokenFamilyWeights[family], true
}

// tokenClassShare is one billing class's contribution to a checkpoint.
// CostPercent is meaningful only when the parent tokenClassShares is Priced.
type tokenClassShare struct {
	Tokens        int `json:"tokens"`
	VolumePercent int `json:"volume_percent"`
	CostPercent   int `json:"cost_percent,omitempty"`
}

// tokenClassBreakdown is the four-class breakdown of a checkpoint's usage. The
// classes are disjoint and sum to Total; Thinking and CacheWrite1h are subsets
// of Output and CacheWrite and are reported alongside, never added in.
//
// Percentages are whole numbers corrected so each set sums to exactly 100 —
// a report that prints 99% is visibly wrong to the reader.
type tokenClassBreakdown struct {
	Input      tokenClassShare `json:"input"`
	CacheWrite tokenClassShare `json:"cache_write"`
	CacheRead  tokenClassShare `json:"cache_read"`
	Output     tokenClassShare `json:"output"`
	Total      int             `json:"total"`

	// Thinking is the part of Output the agent recorded as reasoning.
	Thinking int `json:"thinking,omitempty"`
	// CacheWrite1h is the part of CacheWrite written at the 1-hour TTL, which
	// bills higher than the 5-minute one where the provider charges for it.
	CacheWrite1h int `json:"cache_write_1h,omitempty"`

	// Priced reports whether CostPercent carries meaning. False when the model
	// has no verified ratios, or when the cache-write TTL split is unknown and
	// the provider charges different rates for the two TTLs.
	Priced bool   `json:"priced"`
	Family string `json:"family,omitempty"`
}

// tokenClassShares computes the breakdown. ttlKnown says whether the TTL split
// of cache writes is trustworthy — true for token_usage_version 2 rows, where
// an absent 1-hour figure genuinely means zero, and false for legacy rows,
// where it means "not recorded" and pricing writes would be a guess.
//
// ok=false means there is nothing to report: the caller prints "not recorded"
// rather than four zeros, which would read as a free session.
func tokenClassShares(usage *types.TokenUsage, weights tokenWeights, ttlKnown bool) (tokenClassBreakdown, bool) {
	if usage == nil {
		return tokenClassBreakdown{}, false
	}

	shares := tokenClassBreakdown{
		Input:        tokenClassShare{Tokens: usage.InputTokens},
		CacheWrite:   tokenClassShare{Tokens: usage.CacheCreationTokens},
		CacheRead:    tokenClassShare{Tokens: usage.CacheReadTokens},
		Output:       tokenClassShare{Tokens: usage.OutputTokens},
		Thinking:     usage.ThinkingTokens,
		CacheWrite1h: usage.CacheCreation1hTokens,
		Family:       weights.Family,
	}
	shares.Total = usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens + usage.OutputTokens
	if shares.Total <= 0 {
		return tokenClassBreakdown{}, false
	}

	volumes := []int{usage.InputTokens, usage.CacheCreationTokens, usage.CacheReadTokens, usage.OutputTokens}
	volumePercents := exactPercents(volumes)

	// Cache writes are priced only when the TTL split is known, or when the
	// provider charges one rate for both TTLs (so the split cannot change the
	// answer). Otherwise the whole breakdown stays unpriced rather than
	// silently assuming the cheaper TTL.
	ttlAmbiguous := !ttlKnown && weights.CacheWrite1h != weights.CacheWrite5m
	priced := weights.Family != "" && !ttlAmbiguous

	if priced {
		writes5m := usage.CacheCreationTokens - usage.CacheCreation1hTokens
		if writes5m < 0 {
			writes5m = 0
		}
		costs := []float64{
			float64(usage.InputTokens) * weights.Input,
			float64(writes5m)*weights.CacheWrite5m + float64(usage.CacheCreation1hTokens)*weights.CacheWrite1h,
			float64(usage.CacheReadTokens) * weights.CacheRead,
			float64(usage.OutputTokens) * weights.Output,
		}
		costPercents, anyCost := exactPercentsFloat(costs)
		if anyCost {
			shares.Priced = true
			shares.Input.CostPercent = costPercents[0]
			shares.CacheWrite.CostPercent = costPercents[1]
			shares.CacheRead.CostPercent = costPercents[2]
			shares.Output.CostPercent = costPercents[3]
		}
	}

	shares.Input.VolumePercent = volumePercents[0]
	shares.CacheWrite.VolumePercent = volumePercents[1]
	shares.CacheRead.VolumePercent = volumePercents[2]
	shares.Output.VolumePercent = volumePercents[3]
	return shares, true
}

// exactPercents converts counts to whole percentages summing to exactly 100,
// giving the rounding remainder to the largest fractional parts first. A total
// of zero yields all zeros.
func exactPercents(counts []int) []int {
	values := make([]float64, len(counts))
	for i, c := range counts {
		values[i] = float64(c)
	}
	percents, _ := exactPercentsFloat(values)
	return percents
}

// exactPercentsFloat is exactPercents over weighted values. The bool reports
// whether the values summed to anything at all.
func exactPercentsFloat(values []float64) ([]int, bool) {
	percents := make([]int, len(values))
	var total float64
	for _, v := range values {
		total += v
	}
	if total <= 0 {
		return percents, false
	}

	type remainder struct {
		index int
		frac  float64
	}
	remainders := make([]remainder, 0, len(values))
	assigned := 0
	for i, v := range values {
		exact := v / total * 100
		whole := int(exact)
		percents[i] = whole
		assigned += whole
		remainders = append(remainders, remainder{index: i, frac: exact - float64(whole)})
	}

	// Largest remainder first; ties broken by index so the result is stable.
	sort.SliceStable(remainders, func(a, b int) bool {
		return remainders[a].frac > remainders[b].frac
	})
	for i := 0; assigned < 100 && i < len(remainders); i++ {
		percents[remainders[i].index]++
		assigned++
	}
	return percents, true
}
