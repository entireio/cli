package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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
	// CacheWritePerMTok is the price of cache-write tokens per MTok, if set.
	CacheWritePerMTok *float64 `json:"cache_write_per_mtok"`
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

// add inserts m, replacing any existing entry that shares its id.
func (t *Table) add(m ModelRate) {
	if idx, ok := t.index[m.ID]; ok {
		t.models[idx] = m
		return
	}
	t.index[m.ID] = len(t.models)
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
	if idx, ok := t.index[model]; ok {
		return t.models[idx], true
	}

	norm := strings.ToLower(strings.TrimSpace(model))
	if norm == "" {
		return ModelRate{}, false
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
// cache-write, and output tokens are each priced separately and summed. When r
// omits an explicit cache-read or cache-write rate, the rate is derived from
// InputPerMTok via AnthropicCacheReadMultiplier / AnthropicCacheWriteMultiplier.
func Estimate(r ModelRate, u types.TokenUsage) float64 {
	crRate := AnthropicCacheReadMultiplier * r.InputPerMTok
	if r.CacheReadPerMTok != nil {
		crRate = *r.CacheReadPerMTok
	}
	cwRate := AnthropicCacheWriteMultiplier * r.InputPerMTok
	if r.CacheWritePerMTok != nil {
		cwRate = *r.CacheWritePerMTok
	}

	input := float64(u.InputTokens) * r.InputPerMTok
	cacheRead := float64(u.CacheReadTokens) * crRate
	cacheWrite := float64(u.CacheCreationTokens) * cwRate
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
	return nil
}
