package redact

import (
	"log/slog"
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

// Label constants used in replacement tokens and pattern matching.
const (
	labelEmail   = "EMAIL"
	labelPhone   = "PHONE"
	labelAddress = "ADDRESS"
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

	// patterns holds pre-compiled patterns, populated by ConfigurePII.
	// When nil (e.g., in tests constructing PIIConfig directly),
	// detectPII falls back to compilePIIPatterns.
	patterns []piiPattern
}

// piiPattern is a compiled regex with its replacement token label.
type piiPattern struct {
	regex *regexp.Regexp
	label string // e.g., "EMAIL", "PHONE", "ADDRESS"
}

var (
	piiConfig   *PIIConfig
	piiConfigMu sync.RWMutex
)

// ConfigurePII sets the global PII redaction configuration.
// Pre-compiles patterns so the hot path (String → detectPII) does no compilation.
// Call once at startup after loading settings. Thread-safe.
func ConfigurePII(cfg PIIConfig) {
	piiConfigMu.Lock()
	defer piiConfigMu.Unlock()
	cfgCopy := cfg
	cfgCopy.patterns = compilePIIPatterns(&cfgCopy)
	piiConfig = &cfgCopy
}

// getPIIConfig returns the current PII configuration, or nil if not configured.
func getPIIConfig() *PIIConfig {
	piiConfigMu.RLock()
	defer piiConfigMu.RUnlock()
	return piiConfig
}

// Pre-compiled builtin PII regexes.
var (
	emailRegex = regexp.MustCompile(`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`)
	// phoneRegex uses three branches to avoid false-positives on dotted-decimal
	// strings like version numbers (1.234.567.8901) and IPs (192.168.001.0001).
	// Dots are only allowed as separators when preceded by +1 (unambiguous intl prefix).
	// Without +1, only dashes and spaces are accepted as separators.
	phoneRegex = regexp.MustCompile(
		`(?:` +
			`\+1[-.\s]?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}` + // +1 intl prefix: any separator
			`|` +
			`(?:1[-\s])?\(\d{3}\)\s?\d{3}[-.\s]?\d{4}` + // parenthesized area code
			`|` +
			`(?:1[-\s])?\d{3}[-\s]\d{3}[-\s]\d{4}` + // bare digits: dash/space only
			`)`,
	)
	addressRegex = regexp.MustCompile(`\d{1,5}\s+[A-Z][a-zA-Z]+(?:\s+[A-Z][a-zA-Z]+)*\s+(?:St(?:reet)?|Ave(?:nue)?|Blvd|Boulevard|Dr(?:ive)?|Ln|Lane|Rd|Road|Ct|Court|Pl(?:ace)?|Way|Cir(?:cle)?|Ter(?:race)?|Pkwy|Parkway)\.?`)
)

// emailAllowPatterns are email patterns that should NOT be treated as PII.
// These appear frequently in coding transcripts (git authors, bot accounts)
// and are public metadata rather than private information.
// Entries starting with "@" match the email suffix; entries ending with "@"
// match the email prefix. All comparisons are case-insensitive.
var emailAllowPatterns = []string{
	"noreply@",                  // Generic noreply addresses
	"actions@",                  // GitHub Actions bot
	"@users.noreply.github.com", // GitHub user noreply
	"@noreply.github.com",       // GitHub noreply
}

// isAllowlistedEmail returns true if the email matches a known non-sensitive pattern.
func isAllowlistedEmail(email string) bool {
	lower := strings.ToLower(email)
	for _, pattern := range emailAllowPatterns {
		lp := strings.ToLower(pattern)
		switch {
		case strings.HasPrefix(pattern, "@"):
			if strings.HasSuffix(lower, lp) {
				return true
			}
		case strings.HasSuffix(pattern, "@"):
			if strings.HasPrefix(lower, lp) {
				return true
			}
		default:
			if lower == lp {
				return true
			}
		}
	}
	return false
}

// builtinPIIPattern associates a compiled regex with a category and label.
type builtinPIIPattern struct {
	category PIICategory
	label    string
	regex    *regexp.Regexp
}

// builtinPIIPatterns is the set of default PII detection patterns.
var builtinPIIPatterns = []builtinPIIPattern{
	{PIIEmail, labelEmail, emailRegex},
	{PIIPhone, labelPhone, phoneRegex},
	{PIIAddress, labelAddress, addressRegex},
}

// detectPII returns tagged regions for PII matches in s.
// Returns nil immediately if PII redaction is not configured or not enabled.
func detectPII(cfg *PIIConfig, s string) []taggedRegion {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	patterns := cfg.patterns
	if patterns == nil {
		patterns = compilePIIPatterns(cfg)
	}
	var regions []taggedRegion
	for _, p := range patterns {
		for _, loc := range p.regex.FindAllStringIndex(s, -1) {
			// Skip allowlisted email addresses (noreply, bot accounts, etc.).
			if p.label == labelEmail && isAllowlistedEmail(s[loc[0]:loc[1]]) {
				continue
			}
			regions = append(regions, taggedRegion{
				region: region{loc[0], loc[1]},
				label:  p.label,
			})
		}
	}
	return regions
}

// compilePIIPatterns builds the pattern list from config.
// Builtin regexes are pre-compiled package vars; only custom patterns
// need compilation here.
func compilePIIPatterns(cfg *PIIConfig) []piiPattern {
	var patterns []piiPattern
	for _, bp := range builtinPIIPatterns {
		if enabled, ok := cfg.Categories[bp.category]; ok && enabled {
			patterns = append(patterns, piiPattern{regex: bp.regex, label: bp.label})
		}
	}
	for label, pattern := range cfg.CustomPatterns {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			slog.Warn("skipping invalid custom PII pattern", slog.String("label", label), slog.String("error", err.Error()))
			continue
		}
		patterns = append(patterns, piiPattern{regex: compiled, label: strings.ToUpper(label)})
	}
	return patterns
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
