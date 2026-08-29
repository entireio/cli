package tokenreport

import (
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Provider is the coarse vendor behind a model — for JSON output and notes.
// The pricing row itself is chosen by Family, not Provider.
type Provider string

// Providers with a verified ratio table.
const (
	// ProviderAnthropic covers every Claude model.
	ProviderAnthropic Provider = "anthropic"
	// ProviderOpenAI covers the GPT-5 line.
	ProviderOpenAI Provider = "openai"
	// ProviderGoogle covers the Gemini line.
	ProviderGoogle Provider = "google"
)

// Family selects one row of the price-ratio table. Several model names map to
// the same Family when the provider prices them with identical ratios.
type Family string

// Families with a verified ratio row. See familyWeights for the numbers.
const (
	// FamilyAnthropic is every current Claude model (Fable, Opus, Sonnet,
	// Haiku), including the `[1m]` context suffix.
	FamilyAnthropic Family = "anthropic"
	// FamilyOpenAI8x is gpt-5, gpt-5-mini/nano, gpt-5.1, gpt-5.2 and
	// gpt-5.3-codex: output 8× input.
	FamilyOpenAI8x Family = "openai-8x"
	// FamilyOpenAI6x is gpt-5.4 (incl. -mini/-nano) and gpt-5.5: output 6×
	// input, with a long-context tier.
	FamilyOpenAI6x Family = "openai-6x"
	// FamilyOpenAI56Sol is gpt-5.6-sol: output 5× input and a cache-write
	// charge, with a long-context tier.
	FamilyOpenAI56Sol Family = "openai-5.6-sol"
	// FamilyOpenAI56TerraLuna is gpt-5.6-terra and gpt-5.6-luna: output 6×
	// input and a cache-write charge, with a long-context tier.
	FamilyOpenAI56TerraLuna Family = "openai-5.6-terra-luna"
	// FamilyGemini25Flash is gemini-2.5-flash: output 8.33× input.
	FamilyGemini25Flash Family = "gemini-2.5-flash"
	// FamilyGemini25Pro is gemini-2.5-pro: output 8× input, with a
	// long-context tier.
	FamilyGemini25Pro Family = "gemini-2.5-pro"
	// FamilyGemini35Flash is gemini-3.5-flash: output 6× input.
	FamilyGemini35Flash Family = "gemini-3.5-flash"
	// FamilyGemini36Flash is gemini-3.6-flash and gemini-3.7-flash, which
	// share one row: output 5× input.
	FamilyGemini36Flash Family = "gemini-3.6-flash"
)

// Weights are a model's per-token price ratios relative to its own base input
// price (Input is 1 at the base tier). A zero CacheWrite5m/CacheWrite1h means
// the provider does not charge for cache writes. Weights carry no currency:
// multiplying token counts by them yields dimensionless cost units that are
// only comparable within one model, or across models as shares of a total.
type Weights struct {
	// Provider is the coarse vendor, for JSON and notes.
	Provider Provider `json:"provider"`
	// Family is the ratio row these weights came from; "" for zero Weights.
	Family Family `json:"family"`
	// Input is the ratio for fresh (uncached) input tokens.
	Input float64 `json:"input"`
	// CacheWrite5m is the ratio for cache writes with a 5-minute TTL.
	CacheWrite5m float64 `json:"cache_write_5m"`
	// CacheWrite1h is the ratio for cache writes with a 1-hour TTL. Equal to
	// CacheWrite5m for providers that charge a single cache-write rate.
	CacheWrite1h float64 `json:"cache_write_1h"`
	// CacheRead is the ratio for cache-hit input tokens.
	CacheRead float64 `json:"cache_read"`
	// Output is the ratio for output tokens (thinking tokens included, since
	// they are a subset of output).
	Output float64 `json:"output"`
}

// Price ratios per Family, verified 2026-08-28 against the providers' public
// pricing pages. Re-verify quarterly and bump the date when the numbers are
// re-checked. Every ratio is relative to that model's base input price.
//
//	Anthropic: https://www.anthropic.com/pricing
//	           https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
//	OpenAI:    https://openai.com/api/pricing/
//	Google:    https://ai.google.dev/gemini-api/docs/pricing
//
// Notes carried over from the spec (docs/superpowers/specs/
// 2026-08-28-token-reports-breakdown-first-design.md §3.3):
//   - Anthropic's 1M context is standard pricing on 4.6+, so `[1m]` models
//     take the same row with no multiplier.
//   - OpenAI 8× models list no long-context tier. The 5.4/5.5/5.6 lines
//     charge more above 272,000 input tokens on a call (see longContextTiers).
//   - Gemini 2.5 Flash's cache storage is billed per hour and cannot be
//     derived from token counts, so its cache write is 0 like the other
//     Gemini rows. Only gemini-2.5-pro has a long-context tier (>200,000).
//   - Anthropic fast mode and data-residency uplifts are not detectable from
//     transcripts and are not modelled.
var familyWeights = map[Family]Weights{
	FamilyAnthropic:         {Provider: ProviderAnthropic, Family: FamilyAnthropic, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, Output: 5},
	FamilyOpenAI8x:          {Provider: ProviderOpenAI, Family: FamilyOpenAI8x, Input: 1, CacheWrite5m: 0, CacheWrite1h: 0, CacheRead: 0.1, Output: 8},
	FamilyOpenAI6x:          {Provider: ProviderOpenAI, Family: FamilyOpenAI6x, Input: 1, CacheWrite5m: 0, CacheWrite1h: 0, CacheRead: 0.1, Output: 6},
	FamilyOpenAI56Sol:       {Provider: ProviderOpenAI, Family: FamilyOpenAI56Sol, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 5},
	FamilyOpenAI56TerraLuna: {Provider: ProviderOpenAI, Family: FamilyOpenAI56TerraLuna, Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 1.25, CacheRead: 0.1, Output: 6},
	FamilyGemini25Flash:     {Provider: ProviderGoogle, Family: FamilyGemini25Flash, Input: 1, CacheWrite5m: 0, CacheWrite1h: 0, CacheRead: 0.1, Output: 8.33},
	FamilyGemini25Pro:       {Provider: ProviderGoogle, Family: FamilyGemini25Pro, Input: 1, CacheWrite5m: 0, CacheWrite1h: 0, CacheRead: 0.1, Output: 8},
	FamilyGemini35Flash:     {Provider: ProviderGoogle, Family: FamilyGemini35Flash, Input: 1, CacheWrite5m: 0, CacheWrite1h: 0, CacheRead: 0.1, Output: 6},
	FamilyGemini36Flash:     {Provider: ProviderGoogle, Family: FamilyGemini36Flash, Input: 1, CacheWrite5m: 0, CacheWrite1h: 0, CacheRead: 0.1, Output: 5},
}

// longContextTier is a per-call surcharge applied when a call's input token
// count strictly exceeds AboveInput. Each multiplier scales the corresponding
// base Weights field; CacheWrite scales both TTLs.
type longContextTier struct {
	AboveInput int
	Input      float64
	CacheWrite float64
	CacheRead  float64
	Output     float64
}

// longContextTiers lists the families with a long-context tier, verified on
// the same date as familyWeights. Families absent here price every call at
// the base tier.
var longContextTiers = map[Family]longContextTier{
	// OpenAI gpt-5.4/5.5: 2× input, 2× cached, 1.5× output; no cache-write
	// charge to scale.
	FamilyOpenAI6x: {AboveInput: 272_000, Input: 2, CacheWrite: 1, CacheRead: 2, Output: 1.5},
	// OpenAI gpt-5.6-sol/terra/luna: 2× input, 2× cached, 2× cache write,
	// 1.5× output.
	FamilyOpenAI56Sol:       {AboveInput: 272_000, Input: 2, CacheWrite: 2, CacheRead: 2, Output: 1.5},
	FamilyOpenAI56TerraLuna: {AboveInput: 272_000, Input: 2, CacheWrite: 2, CacheRead: 2, Output: 1.5},
	// Gemini 2.5 Pro: 2× input, 2× cached, 1.5× output.
	FamilyGemini25Pro: {AboveInput: 200_000, Input: 2, CacheWrite: 1, CacheRead: 2, Output: 1.5},
}

// ProviderOf returns the coarse Provider behind a Family, or "" for a Family
// with no ratio row (including the zero Family).
func ProviderOf(f Family) Provider {
	return familyWeights[f].Provider
}

// FamilyFor maps a raw model identifier to its price-ratio Family. Matching is
// case-insensitive on the trimmed name and mirrors cli.formatModel's dispatch
// order (claude, then gpt, then gemini), refined to the rows in familyWeights:
//
//	claude-*                          → FamilyAnthropic
//	gpt-5.6-sol*                      → FamilyOpenAI56Sol
//	gpt-5.6-terra*, gpt-5.6-luna*     → FamilyOpenAI56TerraLuna
//	gpt-5.4*, gpt-5.5*                → FamilyOpenAI6x (incl. -mini/-nano)
//	gpt-5, gpt-5-*, gpt-5.1*, gpt-5.2*, gpt-5.3-codex* → FamilyOpenAI8x
//	gemini-2.5-flash*                 → FamilyGemini25Flash
//	gemini-2.5-pro*                   → FamilyGemini25Pro
//	gemini-3.5-flash*                 → FamilyGemini35Flash
//	gemini-3.6-flash*, gemini-3.7-flash* → FamilyGemini36Flash
//
// Anything else — an empty or unrecorded model, gemini-3-flash-preview, an
// unlisted gpt-5.6 variant, older GPT/Gemini lines — returns ok=false: the
// report shows volume only rather than guessing a price row.
func FamilyFor(model string) (Family, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return "", false
	case strings.HasPrefix(m, "claude-"):
		return FamilyAnthropic, true
	case strings.HasPrefix(m, "gpt-5.6-sol"):
		return FamilyOpenAI56Sol, true
	case strings.HasPrefix(m, "gpt-5.6-terra"), strings.HasPrefix(m, "gpt-5.6-luna"):
		return FamilyOpenAI56TerraLuna, true
	case strings.HasPrefix(m, "gpt-5.4"), strings.HasPrefix(m, "gpt-5.5"):
		return FamilyOpenAI6x, true
	case m == "gpt-5", strings.HasPrefix(m, "gpt-5-"), strings.HasPrefix(m, "gpt-5.1"),
		strings.HasPrefix(m, "gpt-5.2"), strings.HasPrefix(m, "gpt-5.3-codex"):
		return FamilyOpenAI8x, true
	case strings.HasPrefix(m, "gemini-2.5-flash"):
		return FamilyGemini25Flash, true
	case strings.HasPrefix(m, "gemini-2.5-pro"):
		return FamilyGemini25Pro, true
	case strings.HasPrefix(m, "gemini-3.5-flash"):
		return FamilyGemini35Flash, true
	case strings.HasPrefix(m, "gemini-3.6-flash"), strings.HasPrefix(m, "gemini-3.7-flash"):
		return FamilyGemini36Flash, true
	default:
		return "", false
	}
}

// WeightsFor returns the base-tier price ratios for model, the Family that
// supplied them, and ok=true. For a model with no verified ratio row (see
// FamilyFor) it returns zero Weights, "" and ok=false, so callers report
// volume only with a "no verified price ratios" note.
func WeightsFor(model string) (Weights, Family, bool) {
	f, ok := FamilyFor(model)
	if !ok {
		return Weights{}, "", false
	}
	return familyWeights[f], f, true
}

// WeightsForCall returns the price ratios that apply to one call of model
// given inputTokens, that call's input size as the provider counts it toward
// its long-context threshold (the caller passes the right total for the
// provider — OpenAI and Google count cached input too). The result is the
// base-tier Weights, scaled by the Family's long-context tier when
// inputTokens strictly exceeds its threshold (OpenAI 5.4/5.5/5.6 above
// 272,000; Gemini 2.5 Pro above 200,000). Families without a tier, and
// unknown models (zero Weights), are returned unchanged. Tiers are decidable
// only per call, which is why this takes a single call's input rather than a
// session total.
func WeightsForCall(model string, inputTokens int) Weights {
	w, f, ok := WeightsFor(model)
	if !ok {
		return w
	}
	tier, hasTier := longContextTiers[f]
	if !hasTier || inputTokens <= tier.AboveInput {
		return w
	}
	w.Input *= tier.Input
	w.CacheWrite5m *= tier.CacheWrite
	w.CacheWrite1h *= tier.CacheWrite
	w.CacheRead *= tier.CacheRead
	w.Output *= tier.Output
	return w
}

// CostShares is the cost-weighted view of a token usage: each class's share
// of the total cost Units, so a report can say "cache writes were 41% of
// spend" without knowing a currency price. Shares are fractions in [0, 1]
// and Input+CacheWrite+CacheRead+Output sum to 1 when Units > 0.
type CostShares struct {
	// Provider is copied from the Weights used, or "" when a sum mixes
	// providers.
	Provider Provider `json:"provider"`
	// Family is copied from the Weights used, or "" when a sum mixes
	// families.
	Family Family `json:"family"`
	// Input is the fresh-input share of Units.
	Input float64 `json:"input"`
	// CacheWrite is the cache-write share of Units; 0 when CacheWriteUnpriced.
	CacheWrite float64 `json:"cache_write"`
	// CacheRead is the cache-read share of Units.
	CacheRead float64 `json:"cache_read"`
	// Output is the output share of Units, thinking included.
	Output float64 `json:"output"`
	// Thinking is the share of Units spent on thinking tokens. It is a
	// subset view of Output (thinking tokens are priced as output), so it is
	// not part of the sum to 1.
	Thinking float64 `json:"thinking"`
	// Units is the total dimensionless cost: Σ tokens × weight over the four
	// priced classes. Unpriced cache writes contribute nothing.
	Units float64 `json:"units"`
	// CacheWriteUnpriced is true when cache-write tokens were present but
	// could not be priced because the Weights charge different 5m and 1h
	// rates and the usage did not record its 1h split (legacy Anthropic
	// transcripts). Such tokens are excluded from Units rather than blended.
	CacheWriteUnpriced bool `json:"cache_write_unpriced"`
}

// ComputeCostShares weights u by w and returns the resulting shares for a
// usage whose cache-write TTL may be UNKNOWN: summed metadata rows
// (CheckpointSummary / session metadata.json), where CacheCreation1hTokens
// was only recorded from PR #2155 on and, under omitempty, an absent field
// reads as 0. A nil u counts as zero usage. Cache-write units are
// CacheCreation1hTokens×CacheWrite1h + (CacheCreationTokens−CacheCreation1hTokens)×CacheWrite5m,
// except when w prices the two TTLs differently, cache writes were recorded,
// and the 1h split is 0: then the TTL is unknown, the cache-write row is
// marked CacheWriteUnpriced and contributes 0 to Units (never blended — real
// sessions are all-1h or all-5m). Thinking is ThinkingTokens×Output÷Units;
// thinking is inside OutputTokens so it is not added to Units. When Units is
// 0 every share is 0. For usage read from an agent's own per-call usage
// block use ComputeCostSharesKnownTTL.
func ComputeCostShares(u *types.TokenUsage, w Weights) CostShares {
	return computeCostShares(u, w, false)
}

// ComputeCostSharesKnownTTL is ComputeCostShares for a usage whose cache-write
// TTL is KNOWN: it came from an agent-written per-call usage block (the
// TokenAttributor implementations, and subagent records built from a
// subagent's own transcript), so CacheCreation1hTokens is taken at face
// value — 0 means every write was a 5-minute write — and CacheWriteUnpriced
// is never set. Everything else is identical to ComputeCostShares; for a
// Family that prices both TTLs the same, or a usage with a non-zero 1h
// split, the two return the same result.
func ComputeCostSharesKnownTTL(u *types.TokenUsage, w Weights) CostShares {
	return computeCostShares(u, w, true)
}

// computeCostShares is the shared core of the two exported variants; ttlKnown
// selects whether a 0 CacheCreation1hTokens beside recorded cache writes means
// "all 5m" (true) or "not recorded" (false).
func computeCostShares(u *types.TokenUsage, w Weights, ttlKnown bool) CostShares {
	cs := CostShares{Provider: w.Provider, Family: w.Family}
	if u == nil {
		return cs
	}

	inputUnits := float64(u.InputTokens) * w.Input
	cacheReadUnits := float64(u.CacheReadTokens) * w.CacheRead
	outputUnits := float64(u.OutputTokens) * w.Output
	thinkingUnits := float64(u.ThinkingTokens) * w.Output

	var cacheWriteUnits float64
	switch {
	case !ttlKnown && u.CacheCreationTokens > 0 && u.CacheCreation1hTokens == 0 && w.CacheWrite1h != w.CacheWrite5m:
		cs.CacheWriteUnpriced = true
	default:
		// The 1h count is a subset of the total; the 5m remainder is clamped
		// at 0 should the 1h split exceed the total.
		oneHour := u.CacheCreation1hTokens
		fiveMin := max(u.CacheCreationTokens-oneHour, 0)
		cacheWriteUnits = float64(oneHour)*w.CacheWrite1h + float64(fiveMin)*w.CacheWrite5m
	}

	cs.Units = inputUnits + cacheWriteUnits + cacheReadUnits + outputUnits
	if cs.Units <= 0 {
		cs.Units = 0
		return cs
	}
	cs.Input = inputUnits / cs.Units
	cs.CacheWrite = cacheWriteUnits / cs.Units
	cs.CacheRead = cacheReadUnits / cs.Units
	cs.Output = outputUnits / cs.Units
	cs.Thinking = thinkingUnits / cs.Units
	return cs
}

// SumCostShares combines per-call or per-entry shares into one: class units
// (share × Units) are summed and the shares re-derived from the summed Units,
// so a mixed-model report is weighted by cost rather than by call count.
// CacheWriteUnpriced is true if any part was unpriced. Provider and Family
// are kept when every part that names one agrees and cleared to "" otherwise;
// a part with an empty Provider or Family (an unknown model, a bare zero
// value) carries no information and does not vote, regardless of order. With
// no parts or zero total Units every share is 0 (never NaN).
func SumCostShares(parts ...CostShares) CostShares {
	var sum CostShares
	var input, cacheWrite, cacheRead, output, thinking float64
	var providerDisagrees, familyDisagrees bool
	for _, p := range parts {
		input += p.Input * p.Units
		cacheWrite += p.CacheWrite * p.Units
		cacheRead += p.CacheRead * p.Units
		output += p.Output * p.Units
		thinking += p.Thinking * p.Units
		sum.Units += p.Units
		sum.CacheWriteUnpriced = sum.CacheWriteUnpriced || p.CacheWriteUnpriced
		if p.Provider != "" {
			if sum.Provider == "" {
				sum.Provider = p.Provider
			} else if p.Provider != sum.Provider {
				providerDisagrees = true
			}
		}
		if p.Family != "" {
			if sum.Family == "" {
				sum.Family = p.Family
			} else if p.Family != sum.Family {
				familyDisagrees = true
			}
		}
	}
	if providerDisagrees {
		sum.Provider = ""
	}
	if familyDisagrees {
		sum.Family = ""
	}
	if sum.Units <= 0 {
		sum.Units = 0
		return sum
	}
	sum.Input = input / sum.Units
	sum.CacheWrite = cacheWrite / sum.Units
	sum.CacheRead = cacheRead / sum.Units
	sum.Output = output / sum.Units
	sum.Thinking = thinking / sum.Units
	return sum
}
