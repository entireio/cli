package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ModelRate is the price of a single model, in USD per million tokens (MTok).
// CacheReadPerMTok and CacheWritePerMTok are optional: when nil, Estimate
// derives them from InputPerMTok using the Anthropic cache multipliers.
type ModelRate struct {
	// ID is the canonical model identifier (e.g. "claude-opus-4-8").
	ID string `json:"id"`
	// Provider is the vendor that serves the model (e.g. "anthropic").
	Provider string `json:"provider"`
	// Aliases are additional identifiers that resolve to this model. Each may
	// be a literal id or a shell-style glob (e.g. "claude-opus-4-8-*").
	Aliases []string `json:"aliases"`
	// InputPerMTok is the price of fresh (non-cached) input tokens per MTok.
	InputPerMTok float64 `json:"input_per_mtok"`
	// OutputPerMTok is the price of output tokens per MTok.
	OutputPerMTok float64 `json:"output_per_mtok"`
	// CacheReadPerMTok is the price of cache-read tokens per MTok, if set.
	CacheReadPerMTok *float64 `json:"cache_read_per_mtok"`
	// CacheWritePerMTok is the price of 5-minute-TTL cache-write tokens per
	// MTok, if set.
	CacheWritePerMTok *float64 `json:"cache_write_per_mtok"`
	// CacheWrite1hPerMTok is the price of 1-hour-TTL cache-write tokens per
	// MTok, if set. Anthropic prices the two TTLs as independent multiples of
	// the input rate (1.25x / 2.0x) rather than one derived from the other, so
	// a data source that knows the 1h rate should say so explicitly here
	// instead of relying on CacheWritePerMTok to cover both — see Estimate.
	CacheWrite1hPerMTok *float64 `json:"cache_write_1h_per_mtok"`
	// EffectiveDate is the ISO date the price took effect (informational).
	EffectiveDate string `json:"effective_date"`
}

// fileSchema is the on-disk shape of each embedded models/*.json file.
type fileSchema struct {
	SchemaVersion int         `json:"schema_version"`
	Models        []ModelRate `json:"models"`
}

// Table is a resolved set of model rates, keyed for lookup by id and alias.
type Table struct {
	models []ModelRate
	index  map[string]int
}

const (
	// AnthropicCacheReadMultiplier is the fraction of the input price charged
	// for cache-read tokens when a model omits an explicit cache-read rate.
	AnthropicCacheReadMultiplier = 0.1
	// AnthropicCacheWriteMultiplier is the multiple of the input price charged
	// for cache-write tokens when a model omits an explicit cache-write rate.
	AnthropicCacheWriteMultiplier = 1.25
	// AnthropicCacheWrite1hMultiplier is the multiple of the input price charged
	// for 1-hour-TTL cache-write tokens (vs 1.25x for the 5-minute default).
	AnthropicCacheWrite1hMultiplier = 2.0
)

// LoadTable parses every embedded pricing file, validates each entry, then
// layers the given overrides on top: an override whose id matches an existing
// entry replaces it, otherwise it is appended. Overrides are validated too. A
// nil or empty overrides slice yields the embedded defaults unchanged.
func LoadTable(overrides []ModelRate) (*Table, error) {
	entries, err := fs.ReadDir(modelsFS, "models")
	if err != nil {
		return nil, fmt.Errorf("reading embedded pricing models: %w", err)
	}

	t := &Table{index: make(map[string]int)}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := modelsFS.ReadFile(path.Join("models", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading embedded pricing model %s: %w", e.Name(), err)
		}
		var schema fileSchema
		if err := json.Unmarshal(data, &schema); err != nil {
			return nil, fmt.Errorf("parsing pricing model %s: %w", e.Name(), err)
		}
		for i := range schema.Models {
			m := schema.Models[i]
			if err := validateRate(m); err != nil {
				return nil, fmt.Errorf("invalid pricing entry in %s: %w", e.Name(), err)
			}
			t.add(m)
		}
	}

	for i := range overrides {
		o := overrides[i]
		if err := validateRate(o); err != nil {
			return nil, fmt.Errorf("invalid pricing override: %w", err)
		}
		t.add(o)
	}

	return t, nil
}

// ValidOverrides returns the subset of overrides that pass validation, dropping
// and logging each invalid entry rather than failing on it. It mirrors
// LoadRemoteEntries so a single malformed user override in .entire/settings.json
// cannot sink the whole pricing table: LoadTable hard-errors on any invalid
// override, and LoadPricingTable would then discard the entire table and disable
// cost estimation. Callers pre-filter user overrides through this before handing
// them to LoadTable. Order is preserved so LoadTable's last-writer-wins layering
// is unaffected. A nil/empty input yields nil.
func ValidOverrides(ctx context.Context, overrides []ModelRate) []ModelRate {
	if len(overrides) == 0 {
		return nil
	}
	valid := make([]ModelRate, 0, len(overrides))
	for i := range overrides {
		o := overrides[i]
		if err := validateRate(o); err != nil {
			logging.Warn(ctx, "pricing: dropping invalid pricing override entry",
				slog.String("id", o.ID), slog.String("error", err.Error()))
			continue
		}
		valid = append(valid, o)
	}
	return valid
}

// add inserts m, replacing any existing entry that shares its id. Ids are
// canonicalized (trimmed, lower-cased) for the index key so that an override
// whose id differs only in case or surrounding whitespace replaces the builtin
// entry instead of appending a phantom duplicate. The stored id is trimmed. When
// an override omits aliases, provider, or cache rates, it inherits them from
// the replaced entry so a minimal price override (id + input/output only)
// keeps resolving the dated and provider-prefixed spellings the embedded
// entry covered and keeps the provider's cache economics.
func (t *Table) add(m ModelRate) {
	m.ID = strings.TrimSpace(m.ID)
	key := strings.ToLower(m.ID)
	if idx, ok := t.index[key]; ok {
		prev := t.models[idx]
		if len(m.Aliases) == 0 {
			m.Aliases = prev.Aliases
		}
		if strings.TrimSpace(m.Provider) == "" {
			m.Provider = prev.Provider
		}
		if m.CacheReadPerMTok == nil {
			m.CacheReadPerMTok = prev.CacheReadPerMTok
		}
		if m.CacheWritePerMTok == nil {
			m.CacheWritePerMTok = prev.CacheWritePerMTok
		}
		if m.CacheWrite1hPerMTok == nil {
			m.CacheWrite1hPerMTok = prev.CacheWrite1hPerMTok
		}
		t.models[idx] = m
		return
	}
	t.index[key] = len(t.models)
	t.models = append(t.models, m)
}

// Lookup resolves a model identifier to its rate. It first tries an exact match
// against a known id, then falls back to normalized (lower-cased, trimmed)
// matching against ids and aliases, where an alias may be a shell-style glob.
// It performs no implicit model-family fallback; an unknown model returns
// (ModelRate{}, false).
//
// Matching is attempted against the normalized query and, when the query carries
// a bracketed long-context suffix such as "[1m]", the same query with that
// suffix removed. path.Match treats "[...]" as a character class, so a literal
// "[1m]" alias would never match; normalizing on the query side sidesteps that
// trap and lets a bare id like "claude-fable-5[1m]" (the form Claude Code emits)
// resolve to "claude-fable-5", which bills at the same base rate.
func (t *Table) Lookup(model string) (ModelRate, bool) {
	norm := strings.ToLower(strings.TrimSpace(model))
	if norm == "" {
		return ModelRate{}, false
	}

	// The index is keyed by canonical (trimmed, lower-cased) id, so the fast
	// path must probe with the same canonicalization the query was normalized to.
	if idx, ok := t.index[norm]; ok {
		return t.models[idx], true
	}

	candidates := []string{norm}
	if base := stripLongContextSuffix(norm); base != norm && base != "" {
		candidates = append(candidates, base)
	}

	for i := range t.models {
		r := t.models[i]
		id := strings.ToLower(r.ID)
		for _, q := range candidates {
			if id == q {
				return r, true
			}
		}
		for _, alias := range r.Aliases {
			na := strings.ToLower(strings.TrimSpace(alias))
			if na == "" {
				continue
			}
			isGlob := strings.ContainsAny(na, "*?[")
			for _, q := range candidates {
				if isGlob {
					if ok, err := path.Match(na, q); err == nil && ok {
						return r, true
					}
					continue
				}
				if na == q {
					return r, true
				}
			}
		}
	}

	return ModelRate{}, false
}

// stripLongContextSuffix removes a trailing bracketed suffix such as the "[1m]"
// long-context marker that Claude Code appends to model ids (e.g.
// "claude-fable-5[1m]"). It returns s unchanged when there is no such suffix.
func stripLongContextSuffix(s string) string {
	if strings.HasSuffix(s, "]") {
		if i := strings.IndexByte(s, '['); i >= 0 {
			return s[:i]
		}
	}
	return s
}

// Estimate returns the USD cost of u under rate r. Fresh input, cache-read,
// cache-write, and output tokens are each priced separately and summed.
//
// The nil-cache-rate fallbacks are provider-aware. The
// AnthropicCacheReadMultiplier (0.1x input) and AnthropicCacheWriteMultiplier
// (1.25x input) encode Anthropic economics, so they apply only when Provider is
// "anthropic" (case-insensitive). For any other provider a missing cache-read or
// cache-write rate falls back to the full input rate (1.0x) — billing cached
// tokens as normal input, which never undercharges. In practice the non-Anthropic
// embedded tables carry explicit cache rates, so this fallback is a safety net.
//
// The 1-hour-TTL cache-write rate is resolved separately from the 5-minute one:
// CacheWrite1hPerMTok wins when set (a data source that knows the two TTLs bill
// differently should say so explicitly); otherwise an Anthropic entry falls back
// to AnthropicCacheWrite1hMultiplier (2x input) regardless of whether
// CacheWritePerMTok is set, because Anthropic prices the two TTLs as independent
// multiples of input rather than one derived from the other — a 5-minute rate
// supplied without an accompanying 1-hour one (e.g. a remote catalog mirroring
// upstream list prices, which does not distinguish the two) must not silently
// disable the 1-hour premium.
func Estimate(r ModelRate, u types.TokenUsage) float64 {
	isAnthropic := strings.EqualFold(strings.TrimSpace(r.Provider), "anthropic")

	crRate := r.InputPerMTok
	if isAnthropic {
		crRate = AnthropicCacheReadMultiplier * r.InputPerMTok
	}
	if r.CacheReadPerMTok != nil {
		crRate = *r.CacheReadPerMTok
	}
	cwRate := r.InputPerMTok
	if isAnthropic {
		cwRate = AnthropicCacheWriteMultiplier * r.InputPerMTok
	}
	if r.CacheWritePerMTok != nil {
		cwRate = *r.CacheWritePerMTok
	}

	cw1hRate := cwRate
	if isAnthropic {
		cw1hRate = AnthropicCacheWrite1hMultiplier * r.InputPerMTok
	}
	if r.CacheWrite1hPerMTok != nil {
		cw1hRate = *r.CacheWrite1hPerMTok
	}
	cw1h := u.CacheCreation1hTokens
	if cw1h < 0 {
		cw1h = 0
	}
	if cw1h > u.CacheCreationTokens {
		cw1h = u.CacheCreationTokens
	}
	cw5m := u.CacheCreationTokens - cw1h

	input := float64(u.InputTokens) * r.InputPerMTok
	cacheRead := float64(u.CacheReadTokens) * crRate
	cacheWrite := float64(cw5m)*cwRate + float64(cw1h)*cw1hRate
	output := float64(u.OutputTokens) * r.OutputPerMTok

	return (input + cacheRead + cacheWrite + output) / 1e6
}

// validateRate rejects entries that would make an estimate meaningless: a
// missing id or a non-positive input/output rate. Explicit cache rates, when
// present, must not be negative.
func validateRate(r ModelRate) error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("model id must not be empty")
	}
	if r.InputPerMTok <= 0 {
		return fmt.Errorf("model %q: input_per_mtok must be positive, got %v", r.ID, r.InputPerMTok)
	}
	if r.OutputPerMTok <= 0 {
		return fmt.Errorf("model %q: output_per_mtok must be positive, got %v", r.ID, r.OutputPerMTok)
	}
	if r.CacheReadPerMTok != nil && *r.CacheReadPerMTok < 0 {
		return fmt.Errorf("model %q: cache_read_per_mtok must not be negative, got %v", r.ID, *r.CacheReadPerMTok)
	}
	if r.CacheWritePerMTok != nil && *r.CacheWritePerMTok < 0 {
		return fmt.Errorf("model %q: cache_write_per_mtok must not be negative, got %v", r.ID, *r.CacheWritePerMTok)
	}
	if r.CacheWrite1hPerMTok != nil && *r.CacheWrite1hPerMTok < 0 {
		return fmt.Errorf("model %q: cache_write_1h_per_mtok must not be negative, got %v", r.ID, *r.CacheWrite1hPerMTok)
	}
	for _, alias := range r.Aliases {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("model %q: alias must not be empty or whitespace", r.ID)
		}
		// A glob alias with malformed metacharacters (e.g. an unterminated "[")
		// would silently never match at Lookup time (path.Match swallows
		// ErrBadPattern there), so reject it up front where it is a config error.
		if strings.ContainsAny(alias, "*?[") {
			if _, err := path.Match(alias, "probe"); errors.Is(err, path.ErrBadPattern) {
				return fmt.Errorf("model %q: invalid alias glob %q: %w", r.ID, alias, err)
			}
		}
	}
	return nil
}
