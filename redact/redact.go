package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/betterleaks/betterleaks/detect"
	"golang.org/x/sync/errgroup"
)

// secretPattern matches high-entropy strings that may be secrets.
// Note: / is excluded to prevent matching entire file paths as single tokens.
// Base64 values containing / are scored whole by detectSlashedBase64 instead.
var secretPattern = regexp.MustCompile(`[A-Za-z0-9+_=-]{10,}`)

// pathSegmentPattern matches a path component that random base64 almost never
// produces: a word ("refs", "Library") or a long lowercase hex name, as in
// content-addressed caches.
var pathSegmentPattern = regexp.MustCompile(`^(?:[A-Z]?[a-z]{3,}|[0-9a-f]{16,})$`)

// credentialedURIPattern matches URLs that embed userinfo with a password, such
// as postgres://user:pass@host/db or redis://:pass@host/0. These often have
// moderate entropy and are not reliably covered by vendor-specific scanners.
// Group 1 is the password.
var credentialedURIPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{1,31}://[^\s/?#@"'` + "`" + `<>:]*:([^\s/?#@"'` + "`" + `<>]+)@[^\s"'` + "`" + `<>]+`)

// databaseURIPasswordPattern is credentialedURIPattern for database schemes,
// with the password (group 1) also allowed to contain a slash or an
// apostrophe. A slash is invalid unencoded in userinfo, but a hand-written DSN
// still carries the secret. Other schemes keep the strict pattern because
// host:port/path@… is common there (http://localhost:5173/@vite/client).
var databaseURIPasswordPattern = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|rediss?)://[^\s/?#@"'` + "`" + `<>:]*:([^\s?#@"` + "`" + `<>]+)@[^\s"'` + "`" + `<>]+`)

// uriPortPrefixPattern matches a databaseURIPasswordPattern "password" that is
// really a port followed by the end of a quoted list item, as in
// ['postgres://localhost:5432','admin@example.com'].
var uriPortPrefixPattern = regexp.MustCompile(`^[0-9]+',`)

// oracleJDBCPattern and sqlplusLoginPattern match Oracle's user/password@connect
// syntax, which has no password= key. oracleJDBCPattern captures the password,
// which Oracle lets a JDBC URL quote; sqlplusLoginPattern captures the
// user/password@connect token, without the quotes scripts usually put around
// it, and the password.
var (
	oracleJDBCPattern   = regexp.MustCompile(`(?i)\bjdbc:oracle:[a-z0-9]+:[^\s/@"'<>` + "`" + `]+/("[^"]*"|[^\s@"'<>` + "`" + `]+)@[^\s"'<>` + "`" + `]*`)
	sqlplusLoginPattern = regexp.MustCompile(`(?i)\bsqlplus(?:\s+-[a-z]+)*\s+["']?([^\s/@"']+/([^\s@"']+)@[^\s"']+)`)
)

// sasSignaturePattern captures the value of a sig parameter, the HMAC that
// authorizes an Azure SAS URL or token. It is URL-encoded base64, so the
// entropy layer only ever sees fragments of it. Parameter order does not
// matter to a SAS token, so sig may also start a bare token or follow the '='
// of a connection-string setting. SAS parameter names are lowercase.
var sasSignaturePattern = regexp.MustCompile(`(?:^|[?&=\s"'])sig=([A-Za-z0-9%+/=]{20,})`)

// dbPasswordKeyShape matches a DB-prefixed credential key (vendor prefix +
// optional `_word`/`-word` segments + `password`/`passwd`/`pwd`). Used to
// compose both the env-var assignment regex and the JSON-key regex so the
// vendor list stays in one place.
const dbPasswordKeyShape = `(?:db|database|pg|postgres|postgresql|mysql|mariadb|redis|mongo|mongodb|sqlserver|mssql|jdbc)(?:[_-]+[a-z0-9]+)*[_-]*(?:password|passwd|pwd)` //nolint:gosec // regex literal, not a credential

// semicolonConnValue is one value of an ODBC or ADO.NET connection string.
// Unquoted values run to the next ';' and may contain spaces, but never a
// newline or a leading or trailing blank.
const semicolonConnValue = `(?:\{[^}]*\}|"[^"]*"|'[^']*'|[^=;"'\s](?:[^=;"'\r\n]*[^=;"'\s])?)`

var (
	jdbcPattern          = regexp.MustCompile(`(?i)\bjdbc:[^\s"'<>` + "`" + `]+`)
	databaseURLPattern   = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|redis)://[^\s"'<>` + "`" + `]+`)
	keywordDSNPattern    = regexp.MustCompile(`(?i)\b[a-z_][a-z0-9_]*=(?:"[^"]*"|'[^']*'|[^\s"']+)(?:\s+[a-z_][a-z0-9_]*=(?:"[^"]*"|'[^']*'|[^\s"']+)){2,}`)
	semicolonConnPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9 _-]*=[ \t]*` + semicolonConnValue + `(?:;[ \t]*[a-z][a-z0-9 _-]*=[ \t]*` + semicolonConnValue + `){2,}`)
	// credentialValuePattern requires the prefix to start at a non-alphanumeric
	// boundary, so APP_DB_PASSWORD matches via the leading `_` but mydbpassword
	// does not. Only spaces and tabs may surround `=`, so a blank value never
	// takes the next line as its value.
	credentialValuePattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(` + dbPasswordKeyShape + `)[ \t]*=[ \t]*("[^"]*"|'[^']*'|[^\s,;&]+)`)

	// keywordTargetPattern requires a host or a database name; a Unix-socket
	// DSN such as `user=app password=... dbname=app` has no host.
	keywordTargetPattern     = regexp.MustCompile(`(?i)(?:^|\s)(?:host|hostaddr|dbname)=`)
	keywordUserPattern       = regexp.MustCompile(`(?i)(?:^|\s)user=`)
	semicolonServerPattern   = regexp.MustCompile(`(?i)(?:^|;)\s*(?:server|data source|datasource|addr|address|network address)\s*=`)
	semicolonUserPattern     = regexp.MustCompile(`(?i)(?:^|;)\s*(?:user id|userid|user|uid)\s*=`)
	semicolonPasswordPattern = regexp.MustCompile(`(?i)(?:^|;)[ \t]*(?:password|pwd)[ \t]*=[ \t]*(\{[^}]*\}|"[^"]*"|'[^']*'|[^;]*)`)
	passwordAssignmentRegex  = regexp.MustCompile(`(?i)(?:^|[?&;\s])(password|pwd)=("[^"]*"|'[^']*'|[^&;\s"']+)`)
	// credentialJSONKeyRegex operates on output of normalizeCredentialJSONKey
	// (already lowercased, `-`/` `/`.` → `_`), so the `(?i)` flag is unnecessary.
	credentialJSONKeyRegex  = regexp.MustCompile(`^` + dbPasswordKeyShape + `$`)
	genericPasswordKeyRegex = regexp.MustCompile(`(?i)^(?:password|passwd|pwd)$`)
)

// entropyThreshold is the minimum Shannon entropy for a string to be considered
// a secret. 4.5 was chosen through trial and error: high enough to avoid false
// positives on common words and identifiers, low enough to catch typical API keys
// and tokens which tend to have entropy well above 5.0.
const entropyThreshold = 4.5

// claudeToolUseIDPattern is the exact shape of a Claude Code tool-use id:
// "toolu_01" plus 22 base62 characters. Such ids score just above
// entropyThreshold and appear in free text (e.g. <tool-use-id> in task
// notifications), where the "*id" JSON-key skip cannot protect them.
//
// Anchored and fixed-length on purpose: secretPattern's class includes '_' and
// '-', so a secret glued onto an id forms one longer match, and that longer
// match must stay subject to the entropy check. A prefix rule would let it
// through.
var claudeToolUseIDPattern = regexp.MustCompile(`^toolu_01[A-Za-z0-9]{22}$`)

// isClaudeToolUseID reports whether an entropy-layer match is, in its
// entirety, a Claude Code tool-use id. It exempts the match from the entropy
// layer only; every other layer still scans it.
func isClaudeToolUseID(match string) bool {
	return len(match) == 30 && claudeToolUseIDPattern.MatchString(match)
}

// contentDigestPattern matches a base64 content digest at its exact encoded
// length: a go.sum h1: hash (SHA-256), or a Subresource Integrity, npm
// lockfile, or CSP hash source (sha1-, sha256-, sha384-, sha512-). They hash
// public content but score above entropyThreshold.
var contentDigestPattern = regexp.MustCompile(`\b(?:h1:[A-Za-z0-9+/]{43}=|sha1-[A-Za-z0-9+/]{27}=|sha256-[A-Za-z0-9+/]{43}=|sha384-[A-Za-z0-9+/]{64}|sha512-[A-Za-z0-9+/]{86}==)`)

// contentDigestSpans returns the byte ranges of content digests in s, in
// order. An entropy-layer match lying entirely inside one is exempt from that
// layer only; a secret glued onto a digest makes the match longer than the
// digest, so it is still checked.
func contentDigestSpans(s string) [][]int {
	if !strings.Contains(s, "h1:") && !strings.Contains(s, "sha") {
		return nil
	}
	spans := contentDigestPattern.FindAllStringIndex(s, -1)
	digests := spans[:0]
	for _, span := range spans {
		// More base64 after the span means a longer value whose prefix only
		// has a digest's length. A '/' inside it would split the entropy match,
		// so its high-entropy head would otherwise look exempt.
		if span[1] < len(s) && isBase64OrTokenByte(s[span[1]]) {
			continue
		}
		digests = append(digests, span)
	}
	return digests
}

func isBase64OrTokenByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("+/=_-", c) >= 0
}

// digestCursor answers whether entropy matches lie inside a content digest.
// Matches arrive in increasing order, so it walks the spans forward and passes
// over each one once.
type digestCursor struct {
	spans [][]int
	next  int
}

func (c *digestCursor) contains(start, end int) bool {
	for c.next < len(c.spans) && c.spans[c.next][1] <= start {
		c.next++
	}
	return c.next < len(c.spans) && c.spans[c.next][0] <= start && end <= c.spans[c.next][1]
}

// RedactedPlaceholder is the replacement text used for redacted secrets.
const RedactedPlaceholder = "REDACTED"

// placeholderSecretValues lists lowercase values that should be treated as
// non-secrets when they appear as a credential value: prior redactions
// (REDACTED / [REDACTED] / <REDACTED>), common documentation placeholders,
// and obviously-non-real defaults. Values matched by shape (mask runs,
// `<…>` brackets, `${…}` shell expansion, template expressions like `%s`)
// are handled separately.
var placeholderSecretValues = func() map[string]struct{} {
	lower := strings.ToLower(RedactedPlaceholder)
	values := []string{
		lower, "[" + lower + "]", "<" + lower + ">",
		"changeme", "example", "placeholder",
		"your_password", "your_db_password", "your_secret",
		"secret_here",
	}
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}()

// RedactedBytes represents transcript data that has been through secret
// redaction. Consumers that require pre-redacted input (e.g., compact.Compact,
// checkpoint stores) accept this type to enforce the contract at compile time.
//
// Produced by JSONLBytes (primary constructor) or trusted wrappers for data
// previously persisted by checkpoint writers.
type RedactedBytes struct {
	data []byte
}

// Bytes returns the underlying byte slice.
func (r RedactedBytes) Bytes() []byte {
	return r.data
}

// Len returns the number of bytes in the redacted payload.
func (r RedactedBytes) Len() int {
	return len(r.data)
}

// AlreadyRedacted wraps transcript bytes known to already be redacted by a
// prior write path. Use this ONLY for trusted sources such as persisted
// checkpoint transcripts or controlled test fixtures. For fresh transcript
// input, use JSONLBytes.
func AlreadyRedacted(data []byte) RedactedBytes {
	return RedactedBytes{data: data}
}

var (
	betterleaksDetector     *detect.Detector
	betterleaksDetectorOnce sync.Once
)

func getDetector() *detect.Detector {
	betterleaksDetectorOnce.Do(func() {
		d, err := detect.NewDetectorDefaultConfig()
		if err != nil {
			return
		}
		betterleaksDetector = d
	})
	return betterleaksDetector
}

// region represents a byte range to redact.
type region struct{ start, end int }

// taggedRegion extends region with a label for typed replacement tokens.
// Empty label = secret (produces "REDACTED"). Non-empty = PII (produces "[REDACTED_<LABEL>]").
type taggedRegion struct {
	region

	label string
}

type jsonReplacement struct {
	key      string
	original string
	redacted string
}

type connectionStringRule struct {
	pattern   *regexp.Regexp
	hasSecret func(string) bool
}

var connectionStringRules = []connectionStringRule{
	{pattern: jdbcPattern, hasSecret: hasJDBCPassword},
	{pattern: databaseURLPattern, hasSecret: hasDatabaseURLSecret},
	{pattern: keywordDSNPattern, hasSecret: hasKeywordDSNPassword},
	{pattern: semicolonConnPattern, hasSecret: hasSemicolonConnectionPassword},
}

// String replaces secrets and PII in s using layered detection:
//  1. Entropy-based: high-entropy alphanumeric sequences (threshold 4.5)
//  2. Pattern-based: scanner engines selected via ConfigureScanners —
//     betterleaks regex rules (several hundred known secret formats) and/or the
//     goredact engine; betterleaks-only when unconfigured
//  3. Provider token prefixes: deterministic prefix rules for credential
//     formats betterleaks misses in isolation (e.g. Supabase sb_secret_)
//  4. Credentialed URIs: URLs containing userinfo passwords
//  5. Database connection strings: JDBC, keyword DSNs, semicolon strings, and
//     Oracle user/password@ logins
//  6. User-defined custom rules: configured via ConfigureCustomRules
//  7. Bounded credential key/value pairs: DB_PASSWORD=..., Azure SAS sig=...
//  8. PII detection: email, phone, address patterns (only when configured via ConfigurePII)
//
// A string is redacted if ANY method flags it.
func String(s string) string {
	return applyRegions(s, detectAllLayers(s))
}

// detectAllLayers runs the always-on, opt-in, and scanner-configurable
// detection layers and returns their tagged regions. The OpenAI Privacy Filter
// (the final, network-backed layer) is NOT included — callers that want it
// append detectOPF spans to the result before passing to applyRegions. See
// StringWithPrivacyFilter for the augmented flow.
func detectAllLayers(s string) []taggedRegion {
	var regions []taggedRegion

	// 1. Entropy-based detection (secrets — always on).
	digests := digestCursor{spans: contentDigestSpans(s)}
	for _, loc := range secretPattern.FindAllStringIndex(s, -1) {
		start, end := skipJSONEscapeLetter(s, loc[0]), loc[1]
		if end-start < 10 {
			continue
		}

		if isClaudeToolUseID(s[start:end]) || digests.contains(start, end) {
			continue
		}
		if shannonEntropy(s[start:end]) > entropyThreshold {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	slashedDigests := digestCursor{spans: digests.spans}
	for _, r := range detectSlashedBase64(s) {
		if !slashedDigests.contains(r.start, r.end) {
			regions = append(regions, r)
		}
	}

	// 2. Pattern-based detection via scanner engines (secrets — selected
	// via ConfigureScanners; betterleaks-only when unconfigured).
	if getScanners().betterleaks {
		if d := getDetector(); d != nil {
			seenSecrets := make(map[string]struct{})
			for _, f := range d.DetectString(s) {
				// Placeholder-valued findings (changeme, secret_here, mask runs)
				// stay visible — but only on an exact match: splitting a greedy
				// finding at a placeholder head can leak a real secret in the tail.
				if isPlaceholderSecretValue(f.Secret) {
					continue
				}
				// A finding reports one occurrence, but the search below already
				// covers every copy of its secret. Repeated findings must not
				// rescan the input and accumulate quadratically many regions.
				if _, seen := seenSecrets[f.Secret]; seen {
					continue
				}
				seenSecrets[f.Secret] = struct{}{}
				searchFrom := 0
				for {
					idx := strings.Index(s[searchFrom:], f.Secret)
					if idx < 0 {
						break
					}
					absIdx := searchFrom + idx
					regions = append(regions, taggedRegion{region: region{absIdx, absIdx + len(f.Secret)}})
					searchFrom = absIdx + len(f.Secret)
				}
			}
		}
	}
	// goredact engine findings (only runs when enabled via ConfigureScanners).
	regions = append(regions, detectGoredact(s)...)

	// 3. Provider-specific deterministic token prefixes (secrets — always on).
	// Catches low-entropy credential formats (e.g. Supabase sb_secret_) that
	// the entropy and betterleaks layers miss when captured in isolation.
	regions = append(regions, detectProviderTokens(s)...)

	// 4. Credentialed URIs (secrets — always on). A URI whose password is a
	// placeholder stays intact, like the connection strings in layer 5, unless
	// its query carries a real password.
	for _, loc := range credentialedURIPattern.FindAllStringSubmatchIndex(s, -1) {
		if isPlaceholderSecretValue(s[loc[2]:loc[3]]) && !uriQueryHasPassword(s[loc[0]:loc[1]]) {
			continue
		}
		regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
	}
	regions = append(regions, detectDatabaseURIPasswords(s)...)

	// 5. Database and connection-string detection (secrets — always on).
	regions = append(regions, detectConnectionStrings(s)...)

	// 6. User-defined custom rules (secrets — only runs when configured).
	regions = append(regions, detectCustomRules(getCustomRulesConfig(), s)...)

	// 7. Bounded credential key/value detection (secrets — always on).
	regions = append(regions, detectCredentialValues(s)...)

	// 8. PII detection (opt-in — only runs when configured).
	regions = append(regions, detectPII(getPIIConfig(), s)...)

	return regions
}

// applyRegions sorts, merges, and replaces the given regions in s, returning
// the redacted string. Returns s unchanged when regions is empty.
//
// Zero-length regions are dropped: replacing nothing would insert a token
// into text that no layer flagged.
func applyRegions(s string, regions []taggedRegion) string {
	regions = slices.DeleteFunc(regions, func(r taggedRegion) bool { return r.end <= r.start })
	if len(regions) == 0 {
		return s
	}

	sort.Slice(regions, func(i, j int) bool {
		if regions[i].start != regions[j].start {
			return regions[i].start < regions[j].start
		}
		if regions[i].end != regions[j].end {
			return regions[i].end > regions[j].end // larger region first
		}
		return regions[i].label < regions[j].label // deterministic tie-break
	})
	merged := []taggedRegion{regions[0]}
	for _, r := range regions[1:] {
		last := &merged[len(merged)-1]
		if r.start <= last.end {
			if r.end > last.end {
				last.end = r.end
			}
			// Keep the existing label (first/larger region wins)
		} else {
			merged = append(merged, r)
		}
	}

	var b strings.Builder
	prev := 0
	for _, r := range merged {
		b.WriteString(s[prev:r.start])
		b.WriteString(replacementToken(r.label))
		prev = r.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// skipJSONEscapeLetter returns start moved past the letter of a JSON escape
// sequence the match begins with. Example: in "controller.go\nmodel.go" an
// entropy pattern could match "nmodel", and after replacement the '\' would be
// followed by 'R' from "REDACTED", creating the invalid escape '\R'. Only known
// JSON escape letters are skipped, to avoid trimming real secrets that happen
// to follow a literal backslash in decoded content.
func skipJSONEscapeLetter(s string, start int) int {
	if start > 0 && s[start-1] == '\\' {
		switch s[start] {
		case 'n', 't', 'r', 'b', 'f', 'u', '"', '\\', '/':
			return start + 1
		}
	}
	return start
}

// detectSlashedBase64 scores standard-alphabet base64 runs that contain '/' as
// a whole. secretPattern splits such a secret at each '/', and the pieces often
// fall short of entropyThreshold: an n-character string has at most log2(n)
// bits of entropy, so a piece shorter than 23 characters never exceeds it. Runs
// are found by expanding around each '/' because a regex pass over every string
// cost more than secretPattern's own pass.
func detectSlashedBase64(s string) []taggedRegion {
	var regions []taggedRegion
	for i := 0; i < len(s); {
		slash := strings.IndexByte(s[i:], '/')
		if slash < 0 {
			break
		}
		start, end := i+slash, i+slash+1
		for start > 0 && isBase64Byte(s[start-1]) {
			start--
		}
		for end < len(s) && isBase64Byte(s[end]) {
			end++
		}
		for pad := 0; pad < 2 && end < len(s) && s[end] == '='; pad++ {
			end++
		}
		i = end
		start = skipJSONEscapeLetter(s, start)
		if end-start >= 20 && isSlashedBase64Secret(s, start, end) {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	return regions
}

func isBase64Byte(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/'
}

// isSlashedBase64Secret reports whether s[start:end], a base64 run, is a
// high-entropy value rather than a path. A run glued to a preceding '.', '-',
// '_', '~' or '@' continues a dotted name, an identifier, a home path or a
// package scope.
func isSlashedBase64Secret(s string, start, end int) bool {
	run := s[start:end]
	if !strings.Contains(run, "/") {
		return false
	}
	if start > 0 && strings.IndexByte(".-_~@", s[start-1]) >= 0 {
		return false
	}
	for segment := range strings.SplitSeq(run, "/") {
		if pathSegmentPattern.MatchString(segment) {
			return false
		}
	}
	return shannonEntropy(run) > entropyThreshold
}

// detectDatabaseURIPasswords covers database URIs whose password contains a
// character credentialedURIPattern stops at. Placeholder passwords stay
// visible, as in every other credential layer.
func detectDatabaseURIPasswords(s string) []taggedRegion {
	if !strings.Contains(s, "@") || !strings.Contains(s, "://") {
		return nil
	}
	var regions []taggedRegion
	for _, loc := range databaseURIPasswordPattern.FindAllStringSubmatchIndex(s, -1) {
		password := s[loc[2]:loc[3]]
		if uriPortPrefixPattern.MatchString(password) || !hasNonPlaceholderPasswordValue(password) {
			continue
		}
		regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
	}
	return regions
}

// detectOracleCredentials redacts a JDBC Oracle URL as a unit, like the other
// JDBC strings, and a sqlplus login as its user/password@connect token.
func detectOracleCredentials(s string) []taggedRegion {
	if !strings.Contains(s, "@") {
		return nil
	}
	var regions []taggedRegion
	for _, loc := range oracleJDBCPattern.FindAllStringSubmatchIndex(s, -1) {
		if hasNonPlaceholderPasswordValue(s[loc[2]:loc[3]]) {
			regions = append(regions, taggedRegion{region: region{loc[0], trimConnectionStringEnd(s, loc[0], loc[1])}})
		}
	}
	for _, loc := range sqlplusLoginPattern.FindAllStringSubmatchIndex(s, -1) {
		if hasNonPlaceholderPasswordValue(s[loc[4]:loc[5]]) {
			regions = append(regions, taggedRegion{region: region{loc[2], trimConnectionStringEnd(s, loc[2], loc[3])}})
		}
	}
	return regions
}

func detectConnectionStrings(s string) []taggedRegion {
	regions := detectOracleCredentials(s)
	if !strings.ContainsRune(s, '=') {
		return regions
	}
	for _, rule := range connectionStringRules {
		regions = append(regions, detectConnectionStringRule(s, rule)...)
	}
	return regions
}

func detectConnectionStringRule(s string, rule connectionStringRule) []taggedRegion {
	var regions []taggedRegion
	for _, loc := range rule.pattern.FindAllStringIndex(s, -1) {
		start, end := loc[0], trimConnectionStringEnd(s, loc[0], loc[1])
		if start >= end {
			continue
		}
		if start, ok := connectionStringStart(s, start, end, rule.hasSecret); ok {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	return regions
}

// maxConnectionKeyWords is the most words an ODBC or ADO.NET key has
// ("Multiple Active Result Sets").
const maxConnectionKeyWords = 4

// connectionStringStart returns where the connection string in s[start:end]
// begins. Semicolon-string keys may contain spaces ("Data Source"), so a match
// can begin at the words before the string ("Use Server=..."), and those words
// make the server check fail. A rejected match is retried from the start of
// each of the last maxConnectionKeyWords words of its first key, earliest
// first, so the words before the string stay visible. Bounding the retries
// keeps a long rejected line linear.
func connectionStringStart(s string, start, end int, hasSecret func(string) bool) (int, bool) {
	if hasSecret(s[start:end]) {
		return start, true
	}
	keyEnd := start + max(strings.IndexByte(s[start:end], '='), 0)
	var wordStarts []int
	for i := keyEnd - 1; i > start && len(wordStarts) < maxConnectionKeyWords; i-- {
		if s[i] != ' ' && s[i-1] == ' ' {
			wordStarts = append(wordStarts, i)
		}
	}
	for j := len(wordStarts) - 1; j >= 0; j-- {
		if hasSecret(s[wordStarts[j]:end]) {
			return wordStarts[j], true
		}
	}
	return 0, false
}

func trimConnectionStringEnd(s string, start, end int) int {
	for end > start {
		switch s[end-1] {
		case '.', ',', ';', ':', '!', '?', ')', ']':
			end--
		default:
			return end
		}
	}
	return end
}

func hasJDBCPassword(candidate string) bool {
	if !strings.HasPrefix(strings.ToLower(candidate), "jdbc:") {
		return false
	}
	return hasNonPlaceholderPasswordAssignment(candidate, nil)
}

func hasDatabaseURLSecret(candidate string) bool {
	u, err := url.Parse(candidate)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return hasQueryPasswordSecret(u.Query())
}

// uriQueryHasPassword reports whether a URI's query assigns a real password.
// It parses the query on its own, so a host url.Parse rejects (a templated
// ${DB_HOST}) cannot hide it.
func uriQueryHasPassword(uri string) bool {
	_, query, ok := strings.Cut(uri, "?")
	if !ok {
		return false
	}
	query, _, _ = strings.Cut(query, "#")
	// ParseQuery keeps every pair it can decode even when it reports an
	// error for another.
	values, _ := url.ParseQuery(query) //nolint:errcheck // partial values are what we want
	return hasQueryPasswordSecret(values)
}

func hasQueryPasswordSecret(values url.Values) bool {
	for key, vals := range values {
		if !isPasswordQueryKey(key) {
			continue
		}
		for _, value := range vals {
			if hasNonPlaceholderPasswordValue(value) {
				return true
			}
		}
	}
	return false
}

func isPasswordQueryKey(key string) bool {
	return strings.EqualFold(key, "password") || strings.EqualFold(key, "pwd")
}

func hasKeywordDSNPassword(candidate string) bool {
	return keywordTargetPattern.MatchString(candidate) &&
		keywordUserPattern.MatchString(candidate) &&
		hasNonPlaceholderPasswordAssignment(candidate, isShellWorkingDirectory)
}

// isShellWorkingDirectory reports whether a keyword-DSN assignment is the
// shell's PWD variable, which env output prints uppercase with an absolute
// path. A lowercase pwd= is always a password.
func isShellWorkingDirectory(key, value string) bool {
	return key == "PWD" && (strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/"))
}

func hasSemicolonConnectionPassword(candidate string) bool {
	return semicolonServerPattern.MatchString(candidate) &&
		semicolonUserPattern.MatchString(candidate) &&
		hasNonPlaceholderSemicolonPassword(candidate)
}

// hasNonPlaceholderSemicolonPassword reads each Password/Pwd value up to the
// next ';', since ODBC and ADO.NET values may contain spaces.
func hasNonPlaceholderSemicolonPassword(candidate string) bool {
	for _, m := range semicolonPasswordPattern.FindAllStringSubmatch(candidate, -1) {
		if hasNonPlaceholderPasswordValue(strings.TrimRight(m[1], " \t")) {
			return true
		}
	}
	return false
}

func detectCredentialValues(s string) []taggedRegion {
	var regions []taggedRegion
	for _, loc := range credentialValuePattern.FindAllStringSubmatchIndex(s, -1) {
		if len(loc) < 6 || loc[4] < 0 || loc[5] < 0 {
			continue
		}
		start, end := unquoteRange(s, loc[4], loc[5])
		if start == loc[4] {
			end = extendPastValueSeparators(s, end)
		}
		if hasNonPlaceholderPasswordValue(s[start:end]) {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	if strings.Contains(s, "sig=") {
		for _, loc := range sasSignaturePattern.FindAllStringSubmatchIndex(s, -1) {
			regions = append(regions, taggedRegion{region: region{loc[2], loc[3]}})
		}
	}
	return regions
}

// extendPastValueSeparators continues an unquoted credential value across ',',
// ';' and '&' while the text after the separator is not another key=value
// pair, so "ab;cd" is redacted whole while "x;DB_USER=bob" and
// "x&sslmode=require" still stop at the separator. A run of separators counts
// as one. Like the value itself, it never crosses whitespace.
func extendPastValueSeparators(s string, end int) int {
	for end < len(s) && strings.IndexByte(",;&", s[end]) >= 0 {
		next := end + 1
		for next < len(s) && strings.IndexByte(",;&", s[next]) >= 0 {
			next++
		}
		rest := s[next:]
		n := strings.IndexAny(rest, " \t\n\f\r,;&")
		if n < 0 {
			n = len(rest)
		}
		if n == 0 || strings.Contains(rest[:n], "=") {
			break
		}
		end = next + n
	}
	return end
}

func unquoteRange(s string, start, end int) (int, int) {
	if end-start < 2 {
		return start, end
	}
	first, last := s[start], s[end-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return start + 1, end - 1
	}
	return start, end
}

// hasNonPlaceholderPasswordAssignment reports whether candidate assigns a
// password that is not a placeholder. notPassword, when non-nil, excludes
// assignments that only look like one.
func hasNonPlaceholderPasswordAssignment(candidate string, notPassword func(key, value string) bool) bool {
	for _, loc := range passwordAssignmentRegex.FindAllStringSubmatchIndex(candidate, -1) {
		if len(loc) < 6 || loc[4] < 0 || loc[5] < 0 {
			continue
		}
		start, end := unquoteRange(candidate, loc[4], loc[5])
		value := candidate[start:end]
		if notPassword != nil && notPassword(candidate[loc[2]:loc[3]], value) {
			continue
		}
		if hasNonPlaceholderPasswordValue(value) {
			return true
		}
	}
	return false
}

func hasNonPlaceholderPasswordValue(value string) bool {
	return value != "" && !isPlaceholderSecretValue(value)
}

func isPlaceholderSecretValue(value string) bool {
	trimmed := strings.Trim(strings.TrimSpace(value), `"'`)
	if trimmed == "" {
		return true
	}
	if isBracketedPlaceholder(trimmed) {
		return true
	}
	if strings.HasPrefix(trimmed, "${") && strings.HasSuffix(trimmed, "}") {
		return isPlaceholderShellExpansion(trimmed)
	}
	if templatePlaceholderRE.MatchString(trimmed) {
		return true
	}
	normalized := strings.ToLower(trimmed)
	if _, ok := placeholderSecretValues[normalized]; ok {
		return true
	}
	return isRepeatedCharPlaceholder(normalized)
}

// shellExpansionWordRE matches ${NAME<op>word}, whose word becomes the value
// when NAME is unset (-, =) or set (+); with a colon, an empty NAME counts as
// unset. ${NAME:?word} is left out: its word is an error message, never a
// value.
var shellExpansionWordRE = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:?[-=+](.*)\}$`)

// isPlaceholderShellExpansion reports whether a ${…} value is a placeholder:
// one complete expression that is a plain reference, or one whose word is
// itself empty or a placeholder, so ${DB_PASSWORD:-hunter2} and
// ${A}hunter2${B} still count as secrets.
func isPlaceholderShellExpansion(s string) bool {
	if !isSingleBraceExpression(s) {
		return false
	}
	if m := shellExpansionWordRE.FindStringSubmatch(s); m != nil {
		return isPlaceholderSecretValue(m[1])
	}
	return !shellTextOperatorRE.MatchString(s)
}

// shellTextOperatorRE matches ${NAME<op>...} for shell operators whose text can
// carry a literal value: pattern substitution and removal (/ # %), case
// changes (^ ,), and substrings (:offset). Non-shell forms such as ${var.x},
// ${env:X} and ${{secrets.X}} do not match.
var shellTextOperatorRE = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*(?:[/#%^,]|:[-0-9 ])`)

// isSingleBraceExpression reports whether the brace opened by the leading "${"
// of s is closed by its final "}".
func isSingleBraceExpression(s string) bool {
	depth := 0
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i == len(s)-1
			}
		}
	}
	return false
}

// templatePlaceholderRE matches a value that is one interpolation expression:
// a printf verb (%s, %v, %q), a Python %-format name (%(password)s), a format
// field ({} or {password}), or an uppercase shell variable ($DB_PASSWORD).
// Format-field names allow only lowercase letters, `_` and `.`, and shell
// variables only uppercase, so values like {hunter2} or $hunter2 are still
// redacted.
var templatePlaceholderRE = regexp.MustCompile(`^(?:%[svq]|%\([A-Za-z_][A-Za-z0-9_]*\)s|\{\}|\{[a-z_][a-z_.]*\}|\$[A-Z_][A-Z0-9_]*)$`)

// bracketedPlaceholderInteriorRE matches the inside of a "<…>" placeholder
// shape: lowercase letters joined by `-` or `_`. Digits, mixed case, and
// special chars are rejected so values like `<hunter2>` or `<RealPassword>`
// still fall through to redaction.
var bracketedPlaceholderInteriorRE = regexp.MustCompile(`^[a-z][a-z_-]*$`)

// isBracketedPlaceholder reports whether s is a "<name>" doc placeholder
// (e.g. "<password>", "<your-db-password>"). The minimum total length of 5
// keeps this from firing on `<a>` / `<ab>`.
func isBracketedPlaceholder(s string) bool {
	if len(s) < 5 || s[0] != '<' || s[len(s)-1] != '>' {
		return false
	}
	return bracketedPlaceholderInteriorRE.MatchString(s[1 : len(s)-1])
}

// isRepeatedCharPlaceholder reports whether s is a run of a single masking
// character commonly used to redact values in docs and screenshots, e.g.
// "***", "xxxx", "....", "----". The minimum length of 3 keeps single-char
// or 2-char values like `x` or `**` from being treated as masks.
func isRepeatedCharPlaceholder(s string) bool {
	if len(s) < 3 {
		return false
	}
	first := s[0]
	switch first {
	case '*', 'x', '.', '-':
	default:
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != first {
			return false
		}
	}
	return true
}

func isCredentialJSONSecretKey(key string, credentialContext bool) bool {
	normalized := normalizeCredentialJSONKey(key)
	if credentialJSONKeyRegex.MatchString(normalized) {
		return true
	}
	return credentialContext && genericPasswordKeyRegex.MatchString(normalized)
}

func isCredentialJSONObject(obj map[string]any) bool {
	var hasHost, hasUser bool
	for key := range obj {
		switch normalizeCredentialJSONKey(key) {
		case "host", "hostname", "server", "addr", "address", "datasource", "data_source":
			hasHost = true
		case "user", "username", "userid", "user_id", "uid":
			hasUser = true
		}
		if hasHost && hasUser {
			return true
		}
	}
	return false
}

func normalizeCredentialJSONKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	// Flattened-config exporters (Spring, dotnet, Hashicorp Vault) emit dotted keys
	// like "db.password" or "mysql.root.password"; treat them like underscored.
	key = strings.ReplaceAll(key, ".", "_")
	return key
}

// Bytes is a convenience wrapper around String for []byte content.
func Bytes(b []byte) []byte {
	s := string(b)
	redacted := String(s)
	if redacted == s {
		return b
	}
	return []byte(redacted)
}

// JSONLBytes redacts secrets in JSONL-formatted byte content and returns
// the result as RedactedBytes, certifying the output has been through redaction.
// Returns ErrScannerDegraded when the goredact scanner degraded while
// betterleaks is disabled.
func JSONLBytes(b []byte) (RedactedBytes, error) {
	s := string(b)
	redacted, err := JSONLContent(s)
	if err != nil {
		// Degradation outranks a walk error: callers' errors.Is guards must
		// see the sentinel so no error path can reach a Bytes fallback.
		if scannerDegradedSole() {
			return RedactedBytes{}, fmt.Errorf("%w (content walk also failed: %w)", ErrScannerDegraded, err)
		}
		return RedactedBytes{}, err
	}
	if scannerDegradedSole() {
		return RedactedBytes{}, ErrScannerDegraded
	}
	if redacted == s {
		return RedactedBytes{data: b}, nil
	}
	return RedactedBytes{data: []byte(redacted)}, nil
}

// JSONLBytesWithPrivacyFilter augments JSONLBytes with the OpenAI Privacy
// Filter. Use only at condensation/export boundaries; per-turn writes must
// use JSONLBytes.
// Returns ErrScannerDegraded when the goredact scanner degraded while
// betterleaks is disabled.
func JSONLBytesWithPrivacyFilter(ctx context.Context, b []byte) (RedactedBytes, error) {
	s := string(b)
	redacted, err := JSONLContentWithPrivacyFilter(ctx, s)
	if err != nil {
		// Degradation outranks a walk error; see JSONLBytes.
		if scannerDegradedSole() {
			return RedactedBytes{}, fmt.Errorf("%w (content walk also failed: %w)", ErrScannerDegraded, err)
		}
		return RedactedBytes{}, err
	}
	if scannerDegradedSole() {
		return RedactedBytes{}, ErrScannerDegraded
	}
	if redacted == s {
		return RedactedBytes{data: b}, nil
	}
	return RedactedBytes{data: []byte(redacted)}, nil
}

// BytesWithPrivacyFilter augments Bytes with the OpenAI Privacy Filter for
// raw (non-JSONL) byte content. Used by checkpoint write paths that handle
// metadata files which may or may not be JSONL.
func BytesWithPrivacyFilter(ctx context.Context, b []byte) []byte {
	s := string(b)
	redacted := StringWithPrivacyFilter(ctx, s)
	if redacted == s {
		return b
	}
	return []byte(redacted)
}

// JSONLContent parses each line as JSON to determine which string values
// need redaction, then performs targeted replacements on the raw JSON bytes.
// Lines with no secrets are returned unchanged, preserving original formatting.
//
// For multi-line JSON content (e.g., pretty-printed single JSON objects like
// OpenCode export), the function first attempts to parse the entire content as
// a single JSON value. This ensures field-aware redaction (which skips ID fields)
// is used instead of falling back to entropy-based detection on raw text lines,
// which would corrupt high-entropy identifiers.
//
// Large content is sharded across goroutines; output is byte-identical either
// way. See jsonlContent.
func JSONLContent(content string) (string, error) {
	return jsonlContentImpl(content, String, concurrencySafeRedactor)
}

// Whether a per-leaf redactor may be called from several goroutines at once.
//
// This is a property of the redactor, not of the entry point that supplies it,
// so it is passed explicitly rather than implied by which internal function a
// caller happens to reach: String is pure and shardable, while the OPF flow's
// collector closures accumulate into a shared map and slice and are not.
const (
	concurrencySafeRedactor   = true
	concurrencyUnsafeRedactor = false
)

// jsonlContentImpl is the body of JSONLContent parameterized by a per-leaf
// redactor. JSONLContent passes String (regex layers only). The OPF-enabled
// flow uses two passes: one with a collector that records leaves and returns
// identity, one with a redactor that combines regex layers with cached OPF
// spans for the recorded leaves.
//
// concurrencySafe reports whether redactor tolerates concurrent calls; when it
// does, large content is sharded across goroutines.
func jsonlContentImpl(content string, redactor func(string) string, concurrencySafe bool) (string, error) {
	if result, handled, err := redactSingleJSONValue(content, redactor); handled {
		return result, err
	}
	lines := strings.Split(content, "\n")
	if !concurrencySafe {
		return redactJSONLLines(lines, redactor)
	}
	return redactJSONLLinesSharded(lines, redactor)
}

// redactSingleJSONValue handles the case where the whole content is one JSON
// value (e.g. pretty-printed single objects like OpenCode export) so
// field-aware redaction is used instead of falling back to entropy-based
// detection on raw text lines, which would corrupt high-entropy identifiers.
//
// handled is false when the content is JSONL, so the caller processes it line by
// line. Neither the shard split nor the incremental prefix cache in
// checkpoint/redact_cache.go applies to the single-value shape.
func redactSingleJSONValue(content string, redactor func(string) string) (result string, handled bool, err error) {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) == 0 {
		return "", false, nil
	}
	parsed, ok := parseSingleJSONValue(trimmed)
	if !ok {
		return "", false, nil
	}
	// Same duplicate-key rule as the line path: a shadowed value is invisible
	// to the decoded tree, so the document is rebuilt structurally instead of
	// spliced.
	if hasDuplicateJSONKeys(trimmed) {
		result, err = reencodeRedacted(content, redactor)
		if err != nil {
			return "", true, err
		}
		return result, true, nil
	}
	repls := collectJSONLReplacements(parsed, redactor)
	result, err = applyJSONReplacements(content, repls)
	if err != nil {
		return "", true, err
	}
	if len(repls) > 0 {
		result, err = ensureReplacementsLanded(content, result, repls, redactor)
		if err != nil {
			return "", true, err
		}
	}
	return result, true, nil
}

// IsLineDelimited reports whether JSONLContent will redact content line by line
// rather than as a single JSON value.
//
// Callers that split content and redact the pieces separately MUST check this
// first. The line path composes -- redact(A+B) == redact(A)+redact(B) for
// newline-terminated A -- because each line is redacted in isolation. The
// single-JSON-value path does NOT: it is field-aware across the whole document,
// so redacting a fragment of it instead falls back to raw regex and entropy
// detection over partial JSON, which is the identifier corruption
// redactSingleJSONValue exists to avoid.
//
// A filename is not a safe proxy for this. OpenCode writes a single JSON object
// to the same full.jsonl path that other agents write JSONL to.
func IsLineDelimited(content []byte) bool {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return false
	}
	// Decode stops at the first complete value, so this does not scan a large
	// JSONL body. Content that is not JSON at all still goes down the line path,
	// where each line is redacted independently.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return true
	}
	return !isSingleJSONValue(dec)
}

// parseSingleJSONValue reports whether s decodes as exactly one JSON value.
//
// Uses a streaming decoder so a large JSONL input is not copied: Decode stops at
// the first complete value, then isSingleJSONValue checks for a second one. A
// decode failure is a routing signal, not an error — it just means the content
// is not a single JSON value.
func parseSingleJSONValue(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, false
	}
	return parsed, isSingleJSONValue(dec)
}

// jsonlShardTargetBytes is the rough content size per shard, and hence also the
// sharding threshold: content under 2x this yields fewer than two shards and
// runs sequentially.
//
// The win is not purely parallelism: redaction cost per byte climbs with input
// size (allocation and GC pressure over one large buffer), so cutting the
// content into small pieces is cheaper per byte *and* lets pieces run
// concurrently. Measured on a 20MB Codex transcript: 43s sequential vs 2.1s
// across 12 shards. Hence a byte-sized shard target rather than one shard per
// core, with worker count bounding actual concurrency.
const jsonlShardTargetBytes = 1 << 20 // 1MiB

// redactJSONLLinesSharded splits lines into contiguous byte-balanced groups,
// redacts them concurrently, and rejoins them in order. Output is byte-identical
// to redactJSONLLines over the same lines, because each line is redacted in
// isolation and shard boundaries fall between lines.
//
// redactor MUST be safe for concurrent use.
func redactJSONLLinesSharded(lines []string, redactor func(string) string) (string, error) {
	// Balance shards by bytes, not by line count: agent transcripts mix
	// thousands of short lines with occasional multi-MB tool results, so equal
	// line counts would leave one oversized shard as the tail.
	bounds := shardLineBounds(lines, jsonlShardTargetBytes)
	if len(bounds) < 2 {
		return redactJSONLLines(lines, redactor)
	}

	results := make([]string, len(bounds))
	g := new(errgroup.Group)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i, b := range bounds {
		g.Go(func() error {
			out, err := redactJSONLLines(lines[b.lo:b.hi], redactor)
			if err != nil {
				return err
			}
			results[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", fmt.Errorf("redacting JSONL shard: %w", err)
	}

	// Shards are contiguous line ranges, so rejoining them with "\n" in order
	// reproduces the sequential join exactly.
	return strings.Join(results, "\n"), nil
}

// lineRange is a half-open range of line indexes forming one shard.
type lineRange struct{ lo, hi int }

// shardLineBounds groups lines into contiguous ranges of roughly targetBytes
// each. A single line larger than targetBytes becomes its own shard rather than
// being split, since lines are the indivisible unit of redaction.
func shardLineBounds(lines []string, targetBytes int) []lineRange {
	var bounds []lineRange
	lo, running := 0, 0
	for i, line := range lines {
		running += len(line) + 1 // +1 for the rejoined newline
		if running >= targetBytes && i+1 < len(lines) {
			bounds = append(bounds, lineRange{lo: lo, hi: i + 1})
			lo, running = i+1, 0
		}
	}
	if lo < len(lines) {
		bounds = append(bounds, lineRange{lo: lo, hi: len(lines)})
	}
	return bounds
}

// redactJSONLLines redacts each line independently and rejoins them with "\n".
//
// Every line is handled in isolation: no state carries between lines, which is
// what makes sharding across goroutines (jsonlContentConcurrent) produce
// byte-identical output to a sequential pass. Keep it that way — a redactor or
// line rule that depended on earlier lines would silently break that guarantee.
func redactJSONLLines(lines []string, redactor func(string) string) (string, error) {
	var b strings.Builder
	// The input size is a capacity hint that avoids repeated doubling of a
	// large buffer. It is only a hint: splicing shrinks or preserves a line,
	// but the structural re-encode fallback can grow one (Go escapes U+2028
	// and U+2029 unconditionally).
	size := len(lines) - 1
	for _, line := range lines {
		size += len(line)
	}
	if size > 0 {
		b.Grow(size)
	}
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		lineTrimmed := strings.TrimSpace(line)
		if lineTrimmed == "" {
			b.WriteString(line)
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(lineTrimmed), &parsed); err != nil {
			b.WriteString(redactor(line))
			continue
		}
		// Duplicate keys make the decoded tree an incomplete view of the raw
		// text (a shadowed value could hide a secret from both the splice and
		// its verification), so such a line skips the splice entirely and is
		// rebuilt structurally, dropping shadowed values.
		if hasDuplicateJSONKeys(lineTrimmed) {
			result, err := reencodeRedacted(line, redactor)
			if err != nil {
				return "", err
			}
			b.WriteString(result)
			continue
		}
		repls := collectJSONLReplacements(parsed, redactor)
		result, err := applyJSONReplacements(line, repls)
		if err != nil {
			return "", err
		}
		if len(repls) > 0 {
			result, err = ensureReplacementsLanded(line, result, repls, redactor)
			if err != nil {
				return "", err
			}
		}
		b.WriteString(result)
	}
	return b.String(), nil
}

// StringWithPrivacyFilter augments String with the OpenAI Privacy Filter.
// Use only at condensation/export boundaries; per-turn writes must use
// String to avoid the OPF shell-out cost inside the agent loop.
func StringWithPrivacyFilter(ctx context.Context, s string) string {
	regions := detectAllLayers(s)
	regions = append(regions, detectOPF(ctx, getOPFConfig(), s)...)
	return applyRegions(s, regions)
}

// JSONLContentWithPrivacyFilter augments JSONLContent with the OpenAI
// Privacy Filter via batched inference. Walks the content twice: pass 1
// collects unique prose-shaped leaves into a single RedactBatch call;
// pass 2 applies the eight regex layers per leaf plus the cached OPF spans
// for that leaf. One OPF shell-out covers the whole transcript instead of
// one per leaf — without batching, a typical 500-leaf transcript would
// take many minutes per commit.
//
// Falls back to the plain JSONLContent flow when OPF is unconfigured, the
// breaker is tripped, no categories are enabled, or the batch call errors.
func JSONLContentWithPrivacyFilter(ctx context.Context, content string) (string, error) {
	cfg := getOPFConfig()
	if cfg == nil || !cfg.Enabled || cfg.runtime == nil || opfBreakerTripped.Load() {
		return jsonlContentImpl(content, String, concurrencySafeRedactor)
	}
	cats := enabledCategories(cfg)
	if len(cats) == 0 {
		return jsonlContentImpl(content, String, concurrencySafeRedactor)
	}

	// Pass 1: collect eligible (has-space, deduped) leaves. The collector
	// closure returns identity so the JSONL walker doesn't mutate content.
	seen := make(map[string]struct{})
	var inputs []string
	if _, err := jsonlContentImpl(content, func(v string) string {
		if strings.ContainsRune(v, ' ') {
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				inputs = append(inputs, v)
			}
		}
		return v
	}, concurrencyUnsafeRedactor); err != nil {
		return "", err
	}

	// Pass 2: single batched OPF call.
	spansByInput := make(map[string][]Span, len(inputs))
	if len(inputs) > 0 {
		fmt.Fprintln(opfStderr, "→ OpenAI Privacy Filter: scanning transcript…")
		start := time.Now()
		batched, err := cfg.runtime.RedactBatch(ctx, inputs, cats)
		if err != nil {
			handleOPFFailure(ctx, cfg, err)
			return jsonlContentImpl(content, String, concurrencySafeRedactor)
		}
		fmt.Fprintf(opfStderr, "✓ OpenAI Privacy Filter: done (%.1fs)\n", time.Since(start).Seconds())
		// A short return means the runtime gave us fewer span slices than
		// inputs — the tail leaves would receive zero OPF spans and the
		// caller would proceed as if OPF had found nothing. That silently
		// produces under-redacted output and is indistinguishable from a
		// "no PII present" result. Treat as a runtime contract violation:
		// trip the breaker so the pre-push rewrite's post-loop
		// OPFBreakerTripped() check aborts before the Entire-OPF-Applied
		// trailer can be attached to under-redacted commits. The production
		// shell-out always returns len(inputs), so this only fires for a
		// misbehaving custom runtime — but the cost of leaving it dormant
		// is too high for a privacy contract.
		if len(batched) != len(inputs) {
			shortErr := fmt.Errorf("opf runtime returned %d span slices for %d inputs", len(batched), len(inputs))
			handleOPFFailure(ctx, cfg, shortErr)
			return jsonlContentImpl(content, String, concurrencySafeRedactor)
		}
		for i, in := range inputs {
			spansByInput[in] = batched[i]
		}
	}

	// Pass 3: per-leaf regex layers + cached OPF spans.
	return jsonlContentImpl(content, func(v string) string {
		regions := detectAllLayers(v)
		regions = append(regions, opfSpanRegions(v, spansByInput[v], cfg)...)
		return applyRegions(v, regions)
	}, concurrencySafeRedactor)
}

// applyJSONReplacements applies collected (original, redacted) string pairs
// to the raw JSON text, replacing JSON-encoded originals with their redacted forms.
// Returns s unchanged if repls is empty.
func applyJSONReplacements(s string, repls []jsonReplacement) (string, error) {
	if len(repls) == 0 {
		return s, nil
	}
	for _, r := range repls {
		origJSON, err := jsonEncodeString(r.original)
		if err != nil {
			return "", err
		}
		replJSON, err := jsonEncodeString(r.redacted)
		if err != nil {
			return "", err
		}
		if r.key == "" {
			s = strings.ReplaceAll(s, origJSON, replJSON)
			continue
		}
		keyJSON, err := jsonEncodeString(r.key)
		if err != nil {
			return "", err
		}
		s = replaceKeyedJSONValue(s, keyJSON, origJSON, replJSON)
	}
	return s, nil
}

// replaceKeyedJSONValue replaces every occurrence of origJSON that follows
// keyJSON + optional whitespace + ':' + optional whitespace. Restricts
// substitution to value positions so a key's own redacted text is not
// rewritten when it collides with another field's value.
func replaceKeyedJSONValue(s, keyJSON, origJSON, replJSON string) string {
	if !strings.Contains(s, keyJSON) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], keyJSON)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		keyEnd := i + j + len(keyJSON)
		b.WriteString(s[i : i+j])
		b.WriteString(keyJSON)
		p := keyEnd
		for p < len(s) && isJSONWhitespace(s[p]) {
			p++
		}
		if p >= len(s) || s[p] != ':' {
			i = keyEnd
			continue
		}
		p++
		for p < len(s) && isJSONWhitespace(s[p]) {
			p++
		}
		if p+len(origJSON) <= len(s) && s[p:p+len(origJSON)] == origJSON {
			b.WriteString(s[keyEnd:p])
			b.WriteString(replJSON)
			i = p + len(origJSON)
			continue
		}
		i = keyEnd
	}
	return b.String()
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// isSingleJSONValue returns true if the decoder has reached EOF (no more
// top-level values). This distinguishes a single JSON value (e.g., pretty-printed
// object) from JSONL (multiple concatenated values). We attempt a second Decode
// and require io.EOF rather than relying on dec.More(), which is documented for
// use inside arrays/objects and not for top-level value boundaries.
func isSingleJSONValue(dec *json.Decoder) bool {
	var discard json.RawMessage
	return dec.Decode(&discard) == io.EOF
}

// walkRedactableStrings walks a parsed JSON value with the shared JSONL skip
// rules (shouldSkipJSONLField, shouldSkipJSONLObject, credential context),
// applying transform to each eligible string leaf. key is the leaf's object
// key, empty inside arrays and at the root.
//
// A skip key preserves its own string values, including the strings in its
// arrays, but objects beneath it are walked with their own keys. The id rule
// matches any key ending in "id", so {"android":{"apiKey":...}} is not an
// identifier, and {"path":{"env":...}} is not a path.
//
// It returns a copy-on-write tree: containers are re-allocated only along
// paths where transform changed a leaf, and changed reports whether any leaf
// did. Every consumer of the skip rules goes through this one walk. The splice
// collection pass, the post-splice verification, and the structural re-encode
// fallback must all agree on which leaves are redactable, and a second copy of
// the rules is how they would drift apart.
func walkRedactableStrings(v any, transform func(key string, credentialContext bool, val string) string) (any, bool) {
	var walk func(key string, credentialContext, skipStrings bool, v any) (any, bool)
	walk = func(key string, credentialContext, skipStrings bool, v any) (any, bool) {
		switch val := v.(type) {
		case map[string]any:
			if shouldSkipJSONLObject(val) {
				return v, false
			}
			childCredentialContext := credentialContext || isCredentialJSONObject(val)
			var out map[string]any
			for k, child := range val {
				newChild, childChanged := walk(k, childCredentialContext, shouldSkipJSONLField(k), child)
				if !childChanged {
					continue
				}
				if out == nil {
					out = maps.Clone(val)
				}
				out[k] = newChild
			}
			if out == nil {
				return v, false
			}
			return out, true
		case []any:
			var out []any
			for i, child := range val {
				newChild, childChanged := walk("", credentialContext, skipStrings, child)
				if !childChanged {
					continue
				}
				if out == nil {
					out = make([]any, len(val))
					copy(out, val)
				}
				out[i] = newChild
			}
			if out == nil {
				return v, false
			}
			return out, true
		case string:
			if skipStrings {
				return v, false
			}
			redacted := transform(key, credentialContext, val)
			if redacted == val {
				return v, false
			}
			return redacted, true
		default:
			return v, false
		}
	}
	return walk("", false, false, v)
}

// redactLeafValue is the per-leaf redaction rule shared by the splice
// collection pass and the structural re-encode fallback: the placeholder for
// a value whose key reveals a secret the value alone does not, otherwise the
// redactor, then the credential-key placeholder for values it left alone.
//
// The redactor always sees the unchanged value. Rewriting part of it first
// would stop a rule that needs the whole value, such as an anchored custom
// rule, from matching, and expose text it used to redact.
func redactLeafValue(key string, credentialContext bool, val string, redactor func(string) string) string {
	if keyContextFindsMore(key, val) {
		return RedactedPlaceholder
	}
	redacted := redactor(val)
	if redacted == val && isCredentialJSONSecretKey(key, credentialContext) && hasNonPlaceholderPasswordValue(val) {
		redacted = RedactedPlaceholder
	}
	return redacted
}

// credentialKeyWords are stems of the keywords of betterleaks' generic-api-key
// and generic-password rules. Those rules flag a bare value only when a key
// containing one of them precedes it, as in api_key=<value>.
var credentialKeyWords = []string{"access", "api", "auth", "cred", "key", "secret", "token", "passw", "psw", "pw"}

// keyContextFindsMore reports whether scanning val as the "key":"val" text it
// came from flags any part of val that scanning val alone leaves. A leaf
// scanned on its own has lost its key, so without this {"api_key":"<hex>"}
// would pass where api_key=<hex> in prose is redacted. Findings inside the key
// itself do not count.
//
// Only keys containing a credentialKeyWords stem pay for the extra scans.
func keyContextFindsMore(key, val string) bool {
	if val == "" || !containsCredentialKeyWord(key) {
		return false
	}
	prefix := `"` + key + `":"`
	start, end := len(prefix), len(prefix)+len(val)
	var coveredAlone []bool
	for _, r := range detectAllLayers(prefix + val + `"`) {
		s, e := max(r.start, start)-start, min(r.end, end)-start
		if s >= e {
			continue
		}
		if coveredAlone == nil {
			coveredAlone = make([]bool, len(val))
			for _, a := range detectAllLayers(val) {
				for i := a.start; i < a.end; i++ {
					coveredAlone[i] = true
				}
			}
		}
		if slices.Contains(coveredAlone[s:e], false) {
			return true
		}
	}
	return false
}

func containsCredentialKeyWord(key string) bool {
	lower := strings.ToLower(key)
	return slices.ContainsFunc(credentialKeyWords, func(word string) bool {
		return strings.Contains(lower, word)
	})
}

// replKey identifies one (key, original) replacement pair for deduplication.
// A struct, not a delimited string concatenation: JSON strings may contain any
// byte including NUL, so no in-band delimiter is unambiguous, and a collision
// here silently drops a replacement that the post-splice verification then
// cannot know to check for.
type replKey struct {
	key      string
	original string
}

// collectJSONLReplacements walks a parsed JSON value and collects unique
// string replacements via the supplied per-leaf redactor. JSONLContent
// passes String; the OPF-enabled flow passes a closure that combines the
// regex layers with cached batched OPF spans.
func collectJSONLReplacements(v any, redactor func(string) string) []jsonReplacement {
	seen := make(map[replKey]bool)
	var repls []jsonReplacement
	walkRedactableStrings(v, func(key string, credentialContext bool, val string) string {
		redacted := redactLeafValue(key, credentialContext, val, redactor)
		if redacted != val {
			k := replKey{key: key, original: val}
			if !seen[k] {
				seen[k] = true
				repls = append(repls, jsonReplacement{key: key, original: val, redacted: redacted})
			}
		}
		// Collection only. The fast path splices the raw text, so the tree is
		// left unchanged and the copy-on-write walk allocates nothing.
		return val
	})
	return repls
}

// hasDuplicateJSONKeys reports whether any object in the JSON document spells
// the same key twice. encoding/json keeps only the LAST duplicate, so every
// shadowed value is invisible to the decoded tree that drives both the splice
// collection and the post-splice verification, while the raw text still
// carries it. A secret in a shadowed value would ship in certified output, so
// such content must be rebuilt structurally (which drops shadowed values
// outright) rather than spliced. A token error reports false: the caller only
// asks about content that already decoded once.
func hasDuplicateJSONKeys(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	return tokenStreamHasDuplicateKeys(dec)
}

// tokenStreamHasDuplicateKeys consumes exactly one JSON value from dec,
// recursing through containers. Per-object keys stay in a small slice until an
// object exceeds duplicateKeySliceThreshold: transcript objects usually carry
// only a handful of keys, so allocating a map for every object would tax every
// parsed line, while a slice alone becomes quadratic for wide tool output.
func tokenStreamHasDuplicateKeys(dec *json.Decoder) bool {
	const duplicateKeySliceThreshold = 16

	t, err := dec.Token()
	if err != nil {
		return false
	}
	d, ok := t.(json.Delim)
	if !ok {
		return false
	}
	switch d {
	case '{':
		var (
			seenSlice []string
			seenMap   map[string]struct{}
		)
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return false
			}
			k, ok := kt.(string)
			if !ok {
				return false
			}
			if seenMap != nil {
				if _, duplicate := seenMap[k]; duplicate {
					return true
				}
				seenMap[k] = struct{}{}
			} else {
				if slices.Contains(seenSlice, k) {
					return true
				}
				if len(seenSlice) == duplicateKeySliceThreshold {
					// Sized from the constant, not len(seenSlice): they are equal
					// on this branch, and the spilled keys plus k are exactly
					// what the map has to hold.
					seenMap = make(map[string]struct{}, duplicateKeySliceThreshold+1)
					for _, seenKey := range seenSlice {
						seenMap[seenKey] = struct{}{}
					}
					seenMap[k] = struct{}{}
					seenSlice = nil
				} else {
					seenSlice = append(seenSlice, k)
				}
			}
			if tokenStreamHasDuplicateKeys(dec) {
				return true
			}
		}
		if _, err := dec.Token(); err != nil {
			return false
		}
	case '[':
		for dec.More() {
			if tokenStreamHasDuplicateKeys(dec) {
				return true
			}
		}
		if _, err := dec.Token(); err != nil {
			return false
		}
	}
	return false
}

// ensureReplacementsLanded verifies that applyJSONReplacements rewrote every
// collected leaf, re-encoding the content from a redacted tree when it did
// not.
//
// The splice matches the JSON re-encoding of each original leaf against the
// raw text, and JSON admits more than one encoding of the same string. A raw
// U+2028 (Go's encoder escapes it even with HTML escaping off), a \/ escape,
// \uXXXX escapes of ASCII, and ensure_ascii-style escaping of non-ASCII all
// spell the leaf differently in the producer's bytes than in ours, so the
// replacement silently misses and the secret ships in certified RedactedBytes.
// Decoding the spliced result and comparing its leaves against the recorded
// originals catches every such miss without assuming the redactor is
// idempotent.
//
// Formatting fidelity is deliberately sacrificed for exactly the lines the
// splice could not rewrite. Content that cannot be re-encoded fails the
// redaction, so JSONLBytes refuses to certify rather than shipping the line.
func ensureReplacementsLanded(content, spliced string, repls []jsonReplacement, redactor func(string) string) (string, error) {
	if !spliceMissed(spliced, repls) {
		return spliced, nil
	}
	return reencodeRedacted(content, redactor)
}

// spliceMissed reports whether any eligible string leaf of the spliced content
// still equals a replacement's original, honoring each pair's key scoping. A
// spliced result that no longer parses also counts as a miss, so the
// structural fallback replaces it with well-formed redacted output.
func spliceMissed(spliced string, repls []jsonReplacement) bool {
	var parsed any
	if err := json.Unmarshal([]byte(strings.TrimSpace(spliced)), &parsed); err != nil {
		return true
	}

	// The originals are indexed by their key scope up front, so the check is
	// linear in leaves plus replacements rather than their product. A large
	// object can carry thousands of redactable leaves, and this runs on the
	// redaction hot path for every line that had replacements. The empty key
	// scopes to every leaf. Two map probes per leaf, no per-leaf allocation.
	originalsByKey := make(map[string]map[string]struct{}, len(repls))
	for _, r := range repls {
		set := originalsByKey[r.key]
		if set == nil {
			set = make(map[string]struct{})
			originalsByKey[r.key] = set
		}
		set[r.original] = struct{}{}
	}

	missed := false
	walkRedactableStrings(parsed, func(key string, _ bool, val string) string {
		if missed {
			return val
		}
		if _, ok := originalsByKey[""][val]; ok {
			missed = true
			return val
		}
		if _, ok := originalsByKey[key][val]; ok {
			missed = true
		}
		return val
	})
	return missed
}

// ErrRedactionIncomplete marks content that redaction flagged but could not
// fully rewrite: the splice missed and the structural re-encode failed too.
//
// It exists to keep the "fail rather than certify" contract holding at the
// call sites that fall back to plain-text redaction when JSONLBytes errors.
// That fallback is for content that is not JSONL at all. Content carrying this
// sentinel parsed fine, and the plain-text fallback would scan the very
// variant-encoded spelling the verification caught the splice missing, so
// those callers must fail the write instead. Match with errors.Is.
var ErrRedactionIncomplete = errors.New("redaction incomplete")

// reencodeRedacted rebuilds JSON content the splice could not rewrite: parse,
// redact every eligible leaf through the shared walk, marshal. Numbers are
// decoded with UseNumber so integer literals beyond float64 precision survive
// the round-trip. The content's surrounding whitespace is reattached so
// rejoining lines with "\n" preserves the original layout.
//
// Both failure paths wrap ErrRedactionIncomplete: the content already parsed
// once to get here, so an error is "redaction flagged this and could not
// finish", never "this is not JSONL".
func reencodeRedacted(content string, redactor func(string) string) (string, error) {
	trimmed := strings.TrimSpace(content)
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return "", fmt.Errorf("%w: re-parse content for structural redaction: %w", ErrRedactionIncomplete, err)
	}
	redactedTree, _ := walkRedactableStrings(parsed, func(key string, credentialContext bool, val string) string {
		return redactLeafValue(key, credentialContext, val, redactor)
	})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactedTree); err != nil {
		return "", fmt.Errorf("%w: re-encode redacted content: %w", ErrRedactionIncomplete, err)
	}
	encoded := strings.TrimSuffix(buf.String(), "\n")
	lead := content[:len(content)-len(strings.TrimLeftFunc(content, unicode.IsSpace))]
	trail := content[len(strings.TrimRightFunc(content, unicode.IsSpace)):]
	return lead + encoded + trail, nil
}

// shouldSkipJSONLField returns true if a JSON key's string values should be
// excluded from scanning/redaction. Skips signature fields (any key ending in
// "signature"), ID fields (ending in "id"/"ids"), and common path/directory
// fields. Objects under such a key are still scanned; see walkRedactableStrings.
func shouldSkipJSONLField(key string) bool {
	lower := strings.ToLower(key)

	// Skip signature fields: cryptographic attestations, not secrets. Covers
	// "signature" (Claude Code) and provider variants like "thinkingSignature"
	// (Oh My Pi). Their values are high-entropy base64, so the entropy scanner
	// would otherwise redact them — corrupting extended-thinking signatures and
	// breaking transcript replay ("Invalid `signature` in `thinking` block").
	if strings.HasSuffix(lower, "signature") {
		return true
	}

	// Skip ID fields
	if strings.HasSuffix(lower, "id") || strings.HasSuffix(lower, "ids") {
		return true
	}

	// Skip common path and directory fields from agent transcripts.
	// These appear frequently in tool calls and are structural, not secrets.
	switch lower {
	case "filepath", "file_path", "cwd", "root", "directory", "dir", "path":
		return true
	}

	return false
}

// codexImageTypes are the exact "type" values Codex uses for embedded
// screenshots. Matched by explicit equality (not a "_image" suffix) so an
// unrelated object shape that happens to end in "_image" (e.g. a
// "docker_image"/"container_image" record carrying a real secret in another
// field) is not skipped.
var codexImageTypes = map[string]bool{
	"input_image":  true,
	"output_image": true,
}

// shouldSkipJSONLObject returns true if the object represents an image payload:
// "type":"image" or "type":"image_url" (Claude/OpenAI chat format), one of
// codexImageTypes (Codex embeds screenshots this way), or "type":"base64".
// Image data is high-entropy binary, not a secret — redacting it would
// corrupt the payload.
func shouldSkipJSONLObject(obj map[string]any) bool {
	t, ok := obj["type"].(string)
	if !ok {
		return false
	}
	return strings.HasPrefix(t, "image") || codexImageTypes[t] || t == "base64"
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[byte]int)
	for i := range len(s) {
		freq[s[i]]++
	}
	length := float64(len(s))
	var entropy float64
	for _, count := range freq {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// jsonEncodeString returns the JSON encoding of s without HTML escaping.
func jsonEncodeString(s string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", fmt.Errorf("json encode string: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
