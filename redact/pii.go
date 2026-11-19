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
	PIIIP      PIICategory = "ip"
)

// Label constants used in replacement tokens and pattern matching.
const (
	labelEmail   = "EMAIL"
	labelPhone   = "PHONE"
	labelAddress = "ADDRESS"
	labelIP      = "IP"
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
	label string // e.g., "EMAIL", "PHONE", "ADDRESS", "IP"
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

	// The addressTail* patterns extend a street-line match through the
	// locality that follows it: an optional USPS directional, ", City" (one
	// to three Capitalized words), and a region — US "ST 12345[-6789]" or a
	// UK postcode. They are anchored at the start of the text after the
	// street match and only consulted when addressRegex matched, so a
	// ", City, ST ZIP" in prose is never claimed on its own. A city word
	// allows interior '.'/'-' and an apostrophe only before a capital
	// (O'Fallon, but not the possessive 's); a trailing '.' is trimmed by
	// the caller as sentence punctuation.
	addressDirPrefix = `(?:[ \t]+(?:NE|NW|SE|SW|N|S|E|W)\b)?[ \t]*,`
	addressCityWords = `[A-Z][A-Za-z.-]*(?:'[A-Z][A-Za-z.-]*)*` +
		`(?:[ \t]+[A-Z][A-Za-z.-]*(?:'[A-Z][A-Za-z.-]*)*){0,2}`
	addressUSRegion  = `[A-Z]{2}[ \t]+\d{5}(?:-\d{4})?`
	addressUKRegion  = `[A-Z]{1,2}\d[A-Z\d]?[ \t]+\d[A-Z]{2}`
	addressRegionSep = `(?:,[ \t]*|[ \t]+)`
	// A secondary-unit designator plus its identifier and trailing comma,
	// e.g. "Unit 12," — only valid inside a region-anchored span.
	addressUnitPrefix = `(?:` +
		`(?:Suite|Ste|Unit|Apt|Apartment|Floor|Fl|Room|Rm|Dept|Department|` +
		`Building|Bldg|Bldng|Lot|Space|Box|Po|Pmb|Hangar|Hanger|Slip|Trlr|` +
		`Trailer|Camp|Pier|Dock|Berth)[ \t]+[A-Za-z0-9][A-Za-z0-9-]*[ \t]*,[ \t]*` +
		`)?`

	// City plus a required region: the postcode vouches for the whole span,
	// so a secondary-unit segment may precede the city.
	addressTailFull = regexp.MustCompile(
		`^` + addressDirPrefix + `[ \t]*` + addressUnitPrefix + addressCityWords +
			addressRegionSep + `(?:` + addressUSRegion + `|` + addressUKRegion + `)\b`)
	// Region with no city: ", OR 97477" or ", NW1 6XE".
	addressTailRegion = regexp.MustCompile(
		`^` + addressDirPrefix + `[ \t]*` + `(?:` + addressUSRegion + `|` + addressUKRegion + `)\b`)
	// City alone: ", Springfield" or ", St. Louis". Rejected in code when
	// the first word is a secondary-unit designator.
	addressTailCity = regexp.MustCompile(`^` + addressDirPrefix + `[ \t]*` + addressCityWords)
	// Directional alone: " NW" after the street suffix.
	addressDirOnly = regexp.MustCompile(`^[ \t]+(?:NE|NW|SE|SW|N|S|E|W)\b`)
	// First-word check for the city-only extension.
	addressUnitDesignator = regexp.MustCompile(
		`(?i)^[ \t]*(?:[ \t]+(?:NE|NW|SE|SW|N|S|E|W)\b)?[ \t]*,[ \t]*` +
			`(?:Suite|Ste|Unit|Apt|Apartment|Floor|Fl|Room|Rm|Dept|Department|` +
			`Building|Bldg|Bldng|Lot|Space|Box|Po|Pmb|Hangar|Hanger|Slip|Trlr|` +
			`Trailer|Camp|Pier|Dock|Berth)\b`)
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
	{PIIIP, labelIP, ipv4CandidateRegex},
	{PIIIP, labelIP, ipv6CandidateRegex},
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
		// Most strings hold no address-shaped text; a byte scan rules that
		// out before running the IP regexes over the whole string.
		if (p.regex == ipv4CandidateRegex && !mayContainIPv4(s)) ||
			(p.regex == ipv6CandidateRegex && !mayContainIPv6(s)) {
			continue
		}
		builtinIP := p.regex == ipv4CandidateRegex || p.regex == ipv6CandidateRegex
		builtinAddress := p.regex == addressRegex
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
			// The built-in IP candidates are only shapes; redactableIP
			// validates them and may trim the match. Matched by regex, not
			// label, so a custom pattern named "ip" is left as written.
			if builtinIP {
				start, end, ok := redactableIP(s, loc[0], loc[1])
				if !ok {
					continue
				}
				loc[0], loc[1] = start, end
			}
			if builtinAddress {
				loc[1] = extendAddressLocality(s, loc[1])
			}
			regions = append(regions, taggedRegion{
				region:    region{loc[0], loc[1]},
				label:     p.label,
				builtinIP: builtinIP,
			})
		}
	}
	return dropNestedIPRegions(regions)
}

// extendAddressLocality returns the end offset of a street-line match
// extended through the locality that follows it. Patterns are tried from
// most to least anchored: city+region, region alone, city alone, then a
// bare directional. A city-only span whose first word is a secondary-unit
// designator (Suite, Unit, ...) is address grammar rather than a locality
// and is rejected; a region-anchored span is always taken because the
// postcode vouches for it.
func extendAddressLocality(s string, end int) int {
	rest := s[end:]
	for _, re := range []*regexp.Regexp{addressTailFull, addressTailRegion, addressTailCity} {
		m := re.FindString(rest)
		if m == "" {
			continue
		}
		if re == addressTailCity && addressUnitDesignator.MatchString(m) {
			break
		}
		m = strings.TrimSuffix(m, ".")
		pos := end + len(m)
		// A '-' + digit right after a ZIP means a truncated ZIP+4; refuse
		// the partial region rather than leak the remainder.
		if pos < len(s) && s[pos] == '-' && pos+1 < len(s) && s[pos+1] >= '0' && s[pos+1] <= '9' {
			return end
		}
		return pos
	}
	if m := addressDirOnly.FindString(rest); m != "" {
		return end + len(m)
	}
	return end
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
