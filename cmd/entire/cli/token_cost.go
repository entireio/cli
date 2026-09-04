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

// Ratio-row identifiers. A family is a set of models that share one price
// shape; the string is surfaced in --json so readers can see which row priced
// a checkpoint.
const (
	priceFamilyAnthropic       = "anthropic"
	priceFamilyOpenAI8x        = "openai-8x"
	priceFamilyOpenAI6x        = "openai-6x"
	priceFamilyOpenAI56Sol     = "openai-5.6-sol"
	priceFamilyOpenAI56TerraLu = "openai-5.6-terra-luna"
	priceFamilyGemini25Flash   = "gemini-2.5-flash"
	priceFamilyGemini25Pro     = "gemini-2.5-pro"
	priceFamilyGemini35Flash   = "gemini-3.5-flash"
	priceFamilyGemini36Flash   = "gemini-3.6-flash"
)

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
var priceFamilyWeights = map[string]tokenWeights{
	priceFamilyAnthropic:       {Family: priceFamilyAnthropic, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, Output: 5},
	priceFamilyOpenAI8x:        {Family: priceFamilyOpenAI8x, Input: 1, CacheRead: 0.1, Output: 8},
	priceFamilyOpenAI6x:        {Family: priceFamilyOpenAI6x, Input: 1, CacheRead: 0.1, Output: 6},
	priceFamilyOpenAI56Sol:     {Family: priceFamilyOpenAI56Sol, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 5},
	priceFamilyOpenAI56TerraLu: {Family: priceFamilyOpenAI56TerraLu, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 6},
	priceFamilyGemini25Flash:   {Family: priceFamilyGemini25Flash, Input: 1, CacheRead: 0.1, Output: 8.33},
	priceFamilyGemini25Pro:     {Family: priceFamilyGemini25Pro, Input: 1, CacheRead: 0.1, Output: 8},
	priceFamilyGemini35Flash:   {Family: priceFamilyGemini35Flash, Input: 1, CacheRead: 0.1, Output: 6},
	priceFamilyGemini36Flash:   {Family: priceFamilyGemini36Flash, Input: 1, CacheRead: 0.1, Output: 5},
}

// priceFamilyForModel maps a recorded model name to a verified ratio row.
// An unrecognised model returns ok=false: callers report volume only rather
// than pricing it against a guessed row.
func priceFamilyForModel(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return "", false
	case strings.HasPrefix(m, "claude-"):
		return priceFamilyAnthropic, true
	case strings.HasPrefix(m, "gpt-5.6-sol"):
		return priceFamilyOpenAI56Sol, true
	case strings.HasPrefix(m, "gpt-5.6-terra"), strings.HasPrefix(m, "gpt-5.6-luna"):
		return priceFamilyOpenAI56TerraLu, true
	case strings.HasPrefix(m, "gpt-5.4"), strings.HasPrefix(m, "gpt-5.5"):
		return priceFamilyOpenAI6x, true
	case m == "gpt-5", strings.HasPrefix(m, "gpt-5-"), strings.HasPrefix(m, "gpt-5.1"),
		strings.HasPrefix(m, "gpt-5.2"), strings.HasPrefix(m, "gpt-5.3-codex"):
		return priceFamilyOpenAI8x, true
	case strings.HasPrefix(m, "gemini-2.5-flash"):
		return priceFamilyGemini25Flash, true
	case strings.HasPrefix(m, "gemini-2.5-pro"):
		return priceFamilyGemini25Pro, true
	case strings.HasPrefix(m, "gemini-3.5-flash"):
		return priceFamilyGemini35Flash, true
	case strings.HasPrefix(m, "gemini-3.6-flash"), strings.HasPrefix(m, "gemini-3.7-flash"):
		return priceFamilyGemini36Flash, true
	default:
		return "", false
	}
}

// tokenWeightsForModel returns the base-tier ratios for model. ok=false means
// the model has no verified row and the report must show volume only.
func tokenWeightsForModel(model string) (tokenWeights, bool) {
	family, ok := priceFamilyForModel(model)
	if !ok {
		return tokenWeights{}, false
	}
	return priceFamilyWeights[family], true
}

// Reasons cost can be withheld. The report prints these verbatim, so each one
// must be true of the case it names — a report that misstates why it withheld a
// number is the same class of error as withholding the wrong one. Because this
// renderer is shared by `checkpoint tokens` and `session tokens`, a reason must
// also not name one command's scope: unpricedUnknownTTL is the single
// deliberate exception, since only a committed checkpoint can predate the
// cache-write TTL field — live state always knows the split.
const (
	unpricedNoModel     = "no model with verified price ratios"
	unpricedMixedModels = "these tokens span models with different price ratios"
	unpricedUnknownTTL  = "this checkpoint predates the cache-write TTL split, which changes the rate"
	unpricedNoCost      = "this provider bills none of these tokens"
)

// tokenClassShare is one billing class's contribution to a checkpoint.
// CostPercent is meaningful only when the parent breakdown is Priced; the key
// is always emitted so a consumer never has to tell "priced but negligible"
// from "unpriced" by the absence of a field.
type tokenClassShare struct {
	Tokens        int `json:"tokens"`
	VolumePercent int `json:"volume_percent"`
	CostPercent   int `json:"cost_percent"`
	// CostZero distinguishes "this class costs exactly nothing on this
	// provider's price sheet" from "its share rounds below one percent", which
	// a CostPercent of 0 cannot express on its own. Several families bill no
	// cache writes at all (openai-6x/8x, the Gemini rows), so a class with real
	// tokens can be genuinely free. Meaningful only when the breakdown is
	// Priced.
	CostZero bool `json:"cost_zero,omitempty"`
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
	Thinking int `json:"thinking"`
	// CacheWrite1h is the part of CacheWrite written at the 1-hour TTL, which
	// bills higher than the 5-minute one where the provider charges for it.
	CacheWrite1h int `json:"cache_write_1h"`

	// Priced reports whether CostPercent carries meaning.
	Priced bool `json:"priced"`
	// UnpricedReason says why cost was withheld, so the report can state the
	// real reason instead of guessing one. Empty when Priced.
	UnpricedReason string `json:"unpriced_reason,omitempty"`
	Family         string `json:"family,omitempty"`
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

	// Subagent usage is nested, and the section above this one reports a total
	// that recurses into it (see totalTokens). Flattening keeps the two totals
	// in one report from disagreeing — subagent tokens were billed in these same
	// four classes.
	flat := flattenTokenUsageForClasses(usage)

	shares := tokenClassBreakdown{
		Input:        tokenClassShare{Tokens: flat.InputTokens},
		CacheWrite:   tokenClassShare{Tokens: flat.CacheCreationTokens},
		CacheRead:    tokenClassShare{Tokens: flat.CacheReadTokens},
		Output:       tokenClassShare{Tokens: flat.OutputTokens},
		Thinking:     flat.ThinkingTokens,
		CacheWrite1h: flat.CacheCreation1hTokens,
		Family:       weights.Family,
	}
	shares.Total = sumTokenClasses(flat)
	if shares.Total <= 0 {
		return tokenClassBreakdown{}, false
	}

	volumes := []int{flat.InputTokens, flat.CacheCreationTokens, flat.CacheReadTokens, flat.OutputTokens}
	volumePercents := exactPercents(volumes)

	// Cache writes are priced only when the TTL split is known, or when the
	// provider charges one rate for both TTLs (so the split cannot change the
	// answer). Otherwise the whole breakdown stays unpriced rather than
	// silently assuming the cheaper TTL.
	// The TTL split only matters when there are cache writes to split and the
	// provider bills the two TTLs differently.
	ttlAmbiguous := !ttlKnown && flat.CacheCreationTokens > 0 && weights.CacheWrite1h != weights.CacheWrite5m

	switch {
	case weights.Family == "":
		shares.UnpricedReason = unpricedNoModel
	case ttlAmbiguous:
		shares.UnpricedReason = unpricedUnknownTTL
	default:
		writes5m := flat.CacheCreationTokens - flat.CacheCreation1hTokens
		if writes5m < 0 {
			writes5m = 0
		}
		costs := []float64{
			float64(flat.InputTokens) * weights.Input,
			float64(writes5m)*weights.CacheWrite5m + float64(flat.CacheCreation1hTokens)*weights.CacheWrite1h,
			float64(flat.CacheReadTokens) * weights.CacheRead,
			float64(flat.OutputTokens) * weights.Output,
		}
		costPercents, anyCost := exactPercentsFloat(costs)
		if !anyCost {
			shares.UnpricedReason = unpricedNoCost
			break
		}
		shares.Priced = true
		shares.Input.CostPercent, shares.Input.CostZero = costPercents[0], costs[0] == 0
		shares.CacheWrite.CostPercent, shares.CacheWrite.CostZero = costPercents[1], costs[1] == 0
		shares.CacheRead.CostPercent, shares.CacheRead.CostZero = costPercents[2], costs[2] == 0
		shares.Output.CostPercent, shares.Output.CostZero = costPercents[3], costs[3] == 0
	}

	shares.Input.VolumePercent = volumePercents[0]
	shares.CacheWrite.VolumePercent = volumePercents[1]
	shares.CacheRead.VolumePercent = volumePercents[2]
	shares.Output.VolumePercent = volumePercents[3]
	return shares, true
}

// sumTokenClasses adds the four billing classes with saturating arithmetic. It
// is the single definition of "the total" for anything that also prints the
// classes: a total from a different walk than the classes is a report showing
// two answers under one label.
func sumTokenClasses(flat types.TokenUsage) int {
	total := saturatingIntAdd(flat.InputTokens, flat.CacheCreationTokens)
	total = saturatingIntAdd(total, flat.CacheReadTokens)
	return saturatingIntAdd(total, flat.OutputTokens)
}

// flattenTokenUsageForClasses folds nested subagent usage into one flat total,
// clamping every field at zero and saturating every sum. Checkpoint metadata
// lives on a branch anyone with push access can write, so this walk applies
// types.MaxSubagentDepth itself rather than relying on whoever built the chain:
// a negative count, a wrapped sum, or an unbounded chain each reach the report
// as a nonsense percentage.
func flattenTokenUsageForClasses(usage *types.TokenUsage) types.TokenUsage {
	var flat types.TokenUsage
	u := usage
	for depth := 0; u != nil && depth < types.MaxSubagentDepth; u, depth = u.SubagentTokens, depth+1 {
		flat.InputTokens = saturatingIntAdd(flat.InputTokens, max(u.InputTokens, 0))
		flat.CacheCreationTokens = saturatingIntAdd(flat.CacheCreationTokens, max(u.CacheCreationTokens, 0))
		flat.CacheCreation1hTokens = saturatingIntAdd(flat.CacheCreation1hTokens, max(u.CacheCreation1hTokens, 0))
		flat.CacheReadTokens = saturatingIntAdd(flat.CacheReadTokens, max(u.CacheReadTokens, 0))
		flat.OutputTokens = saturatingIntAdd(flat.OutputTokens, max(u.OutputTokens, 0))
		flat.ThinkingTokens = saturatingIntAdd(flat.ThinkingTokens, max(u.ThinkingTokens, 0))
	}
	return flat
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
