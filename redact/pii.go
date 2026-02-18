package redact

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

// PIICategory identifies a category of personally identifiable information.
type PIICategory string

const (
	PIIEmail   PIICategory = "email"
	PIIPhone   PIICategory = "phone"
	PIIAddress PIICategory = "address"
)

// PIIConfig controls which PII categories are detected and redacted.
type PIIConfig struct {
	// Enabled globally enables/disables PII redaction.
	// When false, no PII patterns are checked (secrets still redacted).
	Enabled bool

	// Categories maps each PII category to whether it is enabled.
	// Missing keys default to false (disabled).
	Categories map[PIICategory]bool

	// CustomPatterns allows teams to define additional regex patterns.
	// Each key is a label used in the replacement token (uppercased),
	// and each value is a regex pattern string.
	// Example: {"employee_id": `EMP-\d{6}`} produces [REDACTED_EMPLOYEE_ID].
	CustomPatterns map[string]string
}

// piiPattern is a compiled regex with its replacement token label.
type piiPattern struct {
	regex *regexp.Regexp
	label string // e.g., "EMAIL", "PHONE", "ADDRESS"
}

var (
	piiConfig   *PIIConfig
	piiConfigMu sync.Mutex

	compiledPIIPatterns     []piiPattern
	compiledPIIPatternsOnce sync.Once
)

// ConfigurePII sets the global PII redaction configuration.
// Call once at startup after loading settings. Thread-safe.
func ConfigurePII(cfg PIIConfig) {
	piiConfigMu.Lock()
	defer piiConfigMu.Unlock()
	cfgCopy := cfg
	piiConfig = &cfgCopy
	// Reset compiled patterns so they are recompiled with new config.
	compiledPIIPatternsOnce = sync.Once{}
}

// getPIIConfig returns the current PII configuration, or nil if not configured.
func getPIIConfig() *PIIConfig {
	piiConfigMu.Lock()
	defer piiConfigMu.Unlock()
	return piiConfig
}

// builtinPIIPattern associates a regex pattern string with a category and label.
type builtinPIIPattern struct {
	category PIICategory
	label    string
	pattern  string
}

// builtinPIIPatterns returns the default PII detection patterns.
func builtinPIIPatterns() []builtinPIIPattern {
	return []builtinPIIPattern{
		// Email: standard format
		{PIIEmail, "EMAIL", `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`},
		// Phone: US formats (xxx-xxx-xxxx, (xxx) xxx-xxxx, +1xxxxxxxxxx, etc.)
		{PIIPhone, "PHONE", `(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}`},
		// Address: US street address patterns (123 Main St, 456 Elm Avenue, etc.)
		{PIIAddress, "ADDRESS", `\d{1,5}\s+[A-Z][a-zA-Z]+(?:\s+[A-Z][a-zA-Z]+)*\s+(?:St(?:reet)?|Ave(?:nue)?|Blvd|Boulevard|Dr(?:ive)?|Ln|Lane|Rd|Road|Ct|Court|Pl(?:ace)?|Way|Cir(?:cle)?|Ter(?:race)?|Pkwy|Parkway)\.?`},
	}
}

// detectPII returns tagged regions for PII matches in s.
// Returns nil immediately if PII redaction is not configured or not enabled.
func detectPII(s string) []taggedRegion {
	cfg := getPIIConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	patterns := getCompiledPIIPatterns(cfg)
	var regions []taggedRegion
	for _, p := range patterns {
		for _, loc := range p.regex.FindAllStringIndex(s, -1) {
			regions = append(regions, taggedRegion{
				region: region{loc[0], loc[1]},
				label:  p.label,
			})
		}
	}
	return regions
}

// getCompiledPIIPatterns compiles and caches PII patterns based on the current config.
// Patterns are compiled once per ConfigurePII call.
func getCompiledPIIPatterns(cfg *PIIConfig) []piiPattern {
	compiledPIIPatternsOnce.Do(func() {
		var patterns []piiPattern

		for _, bp := range builtinPIIPatterns() {
			if enabled, ok := cfg.Categories[bp.category]; ok && enabled {
				compiled, err := regexp.Compile(bp.pattern)
				if err != nil {
					continue // skip broken patterns silently
				}
				patterns = append(patterns, piiPattern{regex: compiled, label: bp.label})
			}
		}

		// Custom patterns
		for label, pattern := range cfg.CustomPatterns {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[redact] Warning: invalid custom PII pattern %q: %v\n", label, err)
				continue
			}
			patterns = append(patterns, piiPattern{
				regex: compiled,
				label: strings.ToUpper(label),
			})
		}

		compiledPIIPatterns = patterns
	})
	return compiledPIIPatterns
}

// replacementToken returns the redaction placeholder for a given label.
// Empty label (secrets) returns "REDACTED" for backward compatibility.
// Non-empty label (PII) returns "[REDACTED_<LABEL>]".
func replacementToken(label string) string {
	if label == "" {
		return "REDACTED"
	}
	return "[REDACTED_" + label + "]"
}
