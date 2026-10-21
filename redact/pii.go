package redact

import (
	"log/slog"
	"regexp"
	"slices"
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

	// Logger receives diagnostics (invalid CustomPatterns regexes). nil
	// means slog.Default(); the CLI injects its entry-point-initialized
	// logger so warnings land in .entire/logs/.
	Logger *slog.Logger

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
	// The leading \b keeps digits inside a longer number from starting a match;
	// the trailing digit check lives in detectPII, because \b there would also
	// reject an attached extension such as 555-123-4567x89.
	phoneRegex = regexp.MustCompile(
		`(?:` +
			`\+1[-.\s]?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}` + // +1 intl prefix: any separator
			`|` +
			`(?:\b1[-\s])?\(\d{3}\)\s?\d{3}[-.\s]?\d{4}` + // parenthesized area code
			`|` +
			`(?:\b1[-\s])?\b\d{3}[-\s]\d{3}[-\s]\d{4}` + // bare digits: dash/space only
			`)`,
	)
	// addressRegex is word-bounded at both ends so a suffix such as St or Pl
	// cannot match the start of a longer word (Started, Plan).
	addressRegex = regexp.MustCompile(`\b\d{1,5}\s+[A-Z][a-zA-Z]+(?:\s+[A-Z][a-zA-Z]+)*\s+(?:St(?:reet)?|Ave(?:nue)?|Blvd|Boulevard|Dr(?:ive)?|Ln|Lane|Rd|Road|Ct|Court|Pl(?:ace)?|Way|Cir(?:cle)?|Ter(?:race)?|Pkwy|Parkway)\b\.?`)
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

// gitSSHHosts are public git hosts whose SSH login is git@<host>, so the
// address names the login even without remote syntax (ssh -T git@github.com).
var gitSSHHosts = []string{
	"github.com", "ssh.github.com",
	"gitlab.com", "altssh.gitlab.com",
	"bitbucket.org", "altssh.bitbucket.org",
	"codeberg.org",
	"ssh.dev.azure.com",
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

// isGitSSHUser reports whether the email-shaped match s[start:end] is the git
// SSH login of a remote rather than a mailbox: URL userinfo (ssh://git@host),
// an scp-style remote (git@host:path), or a known git host.
func isGitSSHUser(s string, start, end int) bool {
	user, host, _ := strings.Cut(s[start:end], "@")
	if !strings.EqualFold(user, "git") {
		return false
	}
	if strings.HasSuffix(s[:start], "://") {
		return true
	}
	if end < len(s) && s[end] == ':' && isSCPRemotePath(s[end+1:]) {
		return true
	}
	return slices.Contains(gitSSHHosts, strings.ToLower(host))
}

// isSCPRemotePath reports whether rest, the text after git@host:, starts with
// a repository path: one containing '/' or ending in .git. A word or a port
// after the colon ("git@host: thanks", "git@host:8080") is not a remote.
func isSCPRemotePath(rest string) bool {
	if end := strings.IndexAny(rest, " \t\r\n\"'`\\"); end >= 0 {
		rest = rest[:end]
	}
	path := strings.TrimRight(rest, ".,;:!?)]}>")
	return strings.Contains(path, "/") || strings.HasSuffix(path, ".git")
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
			// Skip allowlisted email addresses (noreply, bot accounts, etc.)
			// and the git login of a remote.
			if p.label == labelEmail && (isAllowlistedEmail(s[loc[0]:loc[1]]) || isGitSSHUser(s, loc[0], loc[1])) {
				continue
			}
			// Skip phone-shaped digits that run on into a longer number.
			if p.label == labelPhone && loc[1] < len(s) && s[loc[1]] >= '0' && s[loc[1]] <= '9' {
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
	// Non-nil even when every pattern is rejected: detectPII reads nil as
	// "never compiled" and would recompile, re-warning, on every call.
	patterns := []piiPattern{}
	for _, bp := range builtinPIIPatterns {
		if enabled, ok := cfg.Categories[bp.category]; ok && enabled {
			patterns = append(patterns, piiPattern{regex: bp.regex, label: bp.label})
		}
	}
	logger := loggerOrDefault(cfg.Logger)
	for label, pattern := range cfg.CustomPatterns {
		compiled, err := compileUserPattern(pattern)
		if err != nil {
			logger.Warn("skipping invalid custom PII pattern",
				componentAttr,
				slog.String("label", label),
				compileErrorAttr(err))
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
