package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
)

// configFingerprintVersion is mixed into ConfigFingerprint and MUST be bumped
// whenever the regex layers change behaviour in a way the hashed config below
// cannot see — a new layer, an altered pattern, a different placeholder token,
// or a change to how regions are merged.
//
// Forgetting to bump it means a caller can reuse output redacted by the old
// pipeline, so treat it as part of the pipeline's public contract.
const configFingerprintVersion = 1

// ConfigFingerprint returns a stable hash over everything that affects the
// output of String and JSONLContent (the eight regex layers). Callers that cache
// redacted output use it to tell when a cached result was produced under
// different rules and must be discarded.
//
// Two things it deliberately does NOT cover, because it cannot:
//
//   - The OpenAI Privacy Filter. OPF is a network-backed layer whose output is
//     not a pure function of local config, so cached output must never be shared
//     between OPF and non-OPF paths. Callers that cache are responsible for
//     keying on which pipeline produced the bytes.
//   - The betterleaks ruleset version, which the library does not expose. A
//     caller that caches across CLI upgrades must mix in the binary's own
//     version, which moves whenever the vendored ruleset does.
//
// Safe for concurrent use.
func ConfigFingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n", configFingerprintVersion)

	// Custom rules (inline plus rule packs), compiled into one list by
	// ConfigureCustomRules. Sorted so map iteration order cannot change the hash.
	if cfg := getCustomRulesConfig(); cfg != nil {
		entries := make([]string, 0, len(cfg.rules))
		for _, rule := range cfg.rules {
			pattern := ""
			if rule.regex != nil {
				pattern = rule.regex.String()
			}
			entries = append(entries, rule.label+"\x00"+pattern)
		}
		sort.Strings(entries)
		fmt.Fprintf(h, "custom:%d\n", len(entries))
		for _, e := range entries {
			fmt.Fprintf(h, "%s\n", e)
		}
	} else {
		h.Write([]byte("custom:none\n"))
	}

	// PII layer: enablement, per-category toggles, and user patterns all change
	// what gets replaced. Sorted iteration keeps map order out of the hash.
	if cfg := getPIIConfig(); cfg != nil {
		fmt.Fprintf(h, "pii:%t\n", cfg.Enabled)
		for _, cat := range slices.Sorted(maps.Keys(cfg.Categories)) {
			fmt.Fprintf(h, "cat:%s=%t\n", cat, cfg.Categories[cat])
		}
		for _, label := range slices.Sorted(maps.Keys(cfg.CustomPatterns)) {
			fmt.Fprintf(h, "piipat:%s\x00%s\n", label, cfg.CustomPatterns[label])
		}
	} else {
		h.Write([]byte("pii:none\n"))
	}

	return hex.EncodeToString(h.Sum(nil))
}
