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
	"unicode/utf8"

	"github.com/betterleaks/betterleaks/detect"
	"golang.org/x/sync/errgroup"
)

// secretPattern matches high-entropy strings that may be secrets.
// Note: / is excluded to prevent matching entire file paths as single tokens.
// Base64 values containing / are scored whole by detectSlashedBase64 instead.
var secretPattern = regexp.MustCompile(`[A-Za-z0-9+_=-]{10,}`)

// pathSegmentPattern matches a path component that random base64 almost never
// produces: a word ("refs", "Library"), a long lowercase hex name, as in
// content-addressed caches, or a version tail with a build suffix, as in Go's
// v1.2.10+incompatible module directories.
var pathSegmentPattern = regexp.MustCompile(`^(?:[A-Z]?[a-z]{3,}|[0-9a-f]{16,}|[0-9]+\+[a-z]{3,})$`)

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
// secretValueKeyShape matches credential-style key names. Two alternation
// branches:
//  1. Explicit shapes (api_key, auth_token, client_secret, …) with an
//     optional prefix.
//  2. Bare `secret`/`token` ONLY when preceded by at least one prefix segment
//     (e.g. csrf_token, auth_secret). Standalone `{"token":"…"}` /
//     `{"secret":"…"}` JSON fields are intentionally NOT matched here —
//     entropy + Betterleaks cover real high-entropy values, and bare
//     `token`/`secret` keys appear in many non-credential contexts.
//
// codeIdentRef is a digit-free identifier or dotted member reference, the
// only shape credentialCodeCallPattern accepts in callee and argument position.
const codeIdentRef = `[A-Za-z_]+(?:\.[A-Za-z_]+)*`

const secretValueKeyShape = `(?:(?:[a-z0-9]+[_-]+)*(?:api[_-]*key|auth[_-]*token|access[_-]*token|refresh[_-]*token|client[_-]*secret|consumer[_-]*secret|secret[_-]*key|secret[_-]*access[_-]*key|private[_-]*key|ssh[_-]*key)|(?:[a-z0-9]+[_-]+)+(?:secret|token))` //nolint:gosec // regex literal, not a credential

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
	// credentialAssignmentPattern captures a bounded key candidate and lets the
	// shared key classifier decide whether it is sensitive. Requiring `=` avoids
	// prose, rejecting an initial `=` avoids equality expressions, and newline-
	// free quote branches keep a match inside one command.
	credentialAssignmentPattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?P<key>[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*=[ \t]*(?P<value>"[^"\r\n]*"|'[^'\r\n]*'|\(value hidden\)|[^\s,;&=` + "`" + `][^\s,;&]*)`)
	// credentialCodeCallPattern accepts a call whose callee and arguments are
	// digit-free identifiers or member references (fetch_secret(),
	// self.refresh_token(), get(name, cfg.key)). Digits and literals are
	// excluded on purpose: `decode(c2stbGl2ZS1hYmM)`, `f(hunter2)` and
	// `Pa55w0rd(x)` put the secret itself in call position.
	credentialCodeCallPattern  = regexp.MustCompile(`^` + codeIdentRef + `\((?:` + codeIdentRef + `(?:,[ \t]*` + codeIdentRef + `)*)?\)$`)
	credentialCodeRefPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	goTemplateReferencePattern = regexp.MustCompile(`^\{\{[ \t]*\.[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+[ \t]*\}\}$`)
	// shellStdinSecretPattern matches a printf literal piped into a
	// secret-management command (`printf 'v' | mycli secrets put KEY`). Named
	// captures keep the literal and key contracts stable; bounded, newline-free
	// spans prevent the match from consuming neighboring transcript entries.
	shellStdinSecretPattern = regexp.MustCompile(
		`(?i)\bprintf\s+(?:(?:%[A-Za-z]|'%-?[0-9.]*[A-Za-z](?:\\n)?'|"%-?[0-9.]*[A-Za-z](?:\\n)?")\s+)?(?P<value>"[^"\r\n]{1,512}"|'[^'\r\n]{1,512}'|[^\s|&;]{1,512})\s*\|\s*[^&;\r\n]{0,300}\bsecrets?\s+(?:put|set|add|create)\s+(?P<key>[A-Za-z_][A-Za-z0-9_.-]*)\b`,
	)

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
	secretJSONKeyRegex      = regexp.MustCompile(`^` + secretValueKeyShape + `$`)
	genericPasswordKeyRegex = regexp.MustCompile(`(?i)^(?:password|passwd|pwd)$`)
	// credentialWordInKeyRE spots a secret-bearing word segment anywhere in a
	// normalized key. Generic credential nouns (auth, cred, credential, cert)
	// stay ordinary identifiers unless paired with one of these secret shapes.
	// Used by isNonSecretIdentifierAssignment: an id/account-suffixed key that
	// also names a secret (aws_access_key_id, api_token_id, secret_id) must
	// never suppress entropy-layer redaction.
	credentialWordInKeyRE = regexp.MustCompile(`(?:^|_)(?:key|token|secret|pass|passwd|password|pwd)(?:_|$)`)
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

// nonSecretTokenPrefixes lists key-name prefixes that, when followed by
// `_token`, denote routinely-non-sensitive debug or pagination identifiers
// (e.g. cancel_token, pagination_token). These are excluded from JSON-key
// redaction so they remain readable in transcripts. Real high-entropy values
// still get caught by the entropy detector / Betterleaks via the per-value
// String() call in collectJSONLReplacements. See isKnownNonSecretTokenKey
// for why `_secret` keys are deliberately not exempted.
var nonSecretTokenPrefixes = map[string]struct{}{
	"cancel":       {},
	"continuation": {},
	"cursor":       {},
	"idempotency":  {},
	"next":         {},
	"page":         {},
	"pagination":   {},
	"prev":         {},
	"previous":     {},
	"sync":         {},
}

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
	data   []byte
	counts *Counts
}

// Bytes returns the underlying byte slice.
func (r RedactedBytes) Bytes() []byte {
	return r.data
}

// Len returns the number of bytes in the redacted payload.
func (r RedactedBytes) Len() int {
	return len(r.data)
}

// Counts reports what redaction replaced to produce these bytes, or nil when
// that is unknown.
func (r RedactedBytes) Counts() *Counts {
	return r.counts
}

// WithCounts returns r carrying counts recorded when its bytes were redacted,
// such as those stored with a reused cached prefix.
func (r RedactedBytes) WithCounts(counts *Counts) RedactedBytes {
	r.counts = counts
	return r
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
		d, err := newBetterleaksDetector()
		if err != nil {
			return
		}
		betterleaksDetector = d
	})
	return betterleaksDetector
}

// region represents a byte range to redact.
type region struct{ start, end int }

// taggedRegion extends region with a label for typed replacement tokens and
// the layer that found it.
// Empty label = secret (produces "REDACTED"). Non-empty = PII (produces "[REDACTED_<LABEL>]").
type taggedRegion struct {
	region

	label string
	layer layer
	// builtinIP marks a match of the built-in IP rules. A custom PII pattern
	// named ip shares the IP label but not the rules that depend on context.
	builtinIP bool
}

type jsonReplacement struct {
	key      string
	original string
	redacted string
	// placeholders is how many placeholders redacting original wrote, when
	// the walk counts.
	placeholders int
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
//  7. Shell stdin literals piped into secret-management commands
//  8. Bounded credential key/value pairs: DB_PASSWORD=..., GITHUB_CLIENT_SECRET=..., Azure SAS sig=...
//  9. PII detection: email, phone, address, IP patterns (only when configured via ConfigurePII)
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
	next := 0

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
		if isNonSecretIdentifierAssignment(s[start:end]) {
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
	next = tagFrom(regions, next, layerEntropy)

	// 2. Pattern-based detection via scanner engines (secrets — selected
	// via ConfigureScanners; betterleaks-only when unconfigured).
	if getScanners().betterleaks {
		if d := getDetector(); d != nil {
			seenSecrets := make(map[string]struct{})
			for _, f := range d.DetectString(s) {
				for _, secret := range betterleaksFindingSecrets(f) {
					// Placeholder-valued findings (changeme, secret_here, mask runs)
					// stay visible — but only on an exact match: splitting a greedy
					// finding at a placeholder head can leak a real secret in the tail.
					if isPlaceholderSecretValue(secret) {
						continue
					}
					// A finding reports one occurrence, but the search below already
					// covers every copy of its secret. Repeated findings must not
					// rescan the input and accumulate quadratically many regions.
					if _, seen := seenSecrets[secret]; seen {
						continue
					}
					seenSecrets[secret] = struct{}{}
					searchFrom := 0
					for {
						idx := strings.Index(s[searchFrom:], secret)
						if idx < 0 {
							break
						}
						absIdx := searchFrom + idx
						regions = append(regions, taggedRegion{region: region{absIdx, absIdx + len(secret)}})
						searchFrom = absIdx + len(secret)
					}
				}
			}
		}
	}
	next = tagFrom(regions, next, layerBetterleaks)
	// goredact engine findings (only runs when enabled via ConfigureScanners).
	regions = append(regions, detectGoredact(s)...)
	next = tagFrom(regions, next, layerGoredact)

	// 3. Provider-specific deterministic token prefixes (secrets — always on).
	// Catches low-entropy credential formats (e.g. Supabase sb_secret_) that
	// the entropy and betterleaks layers miss when captured in isolation.
	regions = append(regions, detectProviderTokens(s)...)
	next = tagFrom(regions, next, layerProviderToken)

	// 4. Credentialed URIs (secrets — always on). A URI whose password is a
	// placeholder stays intact, like the connection strings in layer 5, unless
	// its query carries a real password.
	for _, loc := range credentialedURIPattern.FindAllStringSubmatchIndex(s, -1) {
		if isPlaceholderSecretValue(uriPassword(s, loc[0], loc[2], loc[3])) && !uriQueryHasPassword(s[loc[0]:loc[1]]) {
			continue
		}
		regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
	}
	regions = append(regions, detectDatabaseURIPasswords(s)...)
	next = tagFrom(regions, next, layerCredentialedURI)

	// 5. Database and connection-string detection (secrets — always on).
	regions = append(regions, detectConnectionStrings(s)...)
	next = tagFrom(regions, next, layerConnectionString)

	// 6. User-defined custom rules (secrets — only runs when configured).
	regions = append(regions, detectCustomRules(getCustomRulesConfig(), s)...)
	next = tagFrom(regions, next, layerCustom)

	// 7. Shell stdin literals piped into secret-management commands
	// (secrets — always on).
	regions = append(regions, detectShellStdinSecrets(s)...)
	next = tagFrom(regions, next, layerShellStdin)

	// 8. Bounded credential key/value detection (secrets — always on).
	regions = append(regions, detectCredentialValues(s)...)
	next = tagFrom(regions, next, layerCredentialValue)

	// 9. PII detection (opt-in — only runs when configured).
	regions = append(regions, detectPII(getPIIConfig(), s)...)
	tagFrom(regions, next, layerPII)

	return regions
}

// applyRegions sorts, merges, and replaces the given regions in s, returning
// the redacted string. Returns s unchanged when regions is empty.
//
// Zero-length regions are dropped: replacing nothing would insert a token
// into text that no layer flagged.
func applyRegions(s string, regions []taggedRegion) string {
	return redactRegions(s, regions, nil)
}

// redactRegions is applyRegions that also adds what it replaced to t.
func redactRegions(s string, regions []taggedRegion, t *tally) string {
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
	t.addRegions(regions, len(merged))

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
		if end-start >= 20 && isSlashedBase64Secret(s[start:end]) {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	return regions
}

func isBase64Byte(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/'
}

// isSlashedBase64Secret reports whether run, a base64 run, is a
// high-entropy value rather than a path. Only the run's own segments decide:
// the text before it says nothing, since a secret often follows a flag, a key
// prefix or a name (-p<secret>, token_<secret>, user@<secret>).
func isSlashedBase64Secret(run string) bool {
	if !strings.Contains(run, "/") {
		return false
	}
	for segment := range strings.SplitSeq(run, "/") {
		if pathSegmentPattern.MatchString(segment) {
			return false
		}
	}
	return shannonEntropy(run) > entropyThreshold
}

// uriPassword returns the password s[start:end] of the URI that starts at
// s[uriStart], as the placeholder rules read it: for a URI inside a double
// quoted string, as in psql "postgres://app:${PW:-'$FALLBACK'}@db/app", it is
// read as double quotes enclose it (see doubleQuotedText). The quote before
// the URI must open a string that closes after it on its line: in
// psql ""postgres://app:${PW:-'$S3CRET'}@db/app the quotes are an empty
// string, so the single quotes make $S3CRET literal.
func uriPassword(s string, uriStart, start, end int) string {
	if open := uriStart - 1; open >= 0 && opensDoubleQuotedString(s, open) && quotedPieceEnd(s, open) >= 0 {
		return doubleQuotedText(s[start:end])
	}
	return s[start:end]
}

// opensDoubleQuotedString reports whether s[i] is a double quote that opens a
// string, judged by the run of double quotes that ends there: in a run of
// even length, as "", each quote closes the one before it.
func opensDoubleQuotedString(s string, i int) bool {
	n := 0
	for i-n >= 0 && s[i-n] == '"' {
		n++
	}
	return n%2 == 1
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
		password := uriPassword(s, loc[0], loc[2], loc[3])
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

func detectShellStdinSecrets(s string) []taggedRegion {
	if !strings.ContainsRune(s, '|') || !containsFoldASCII(s, "printf") || !containsFoldASCII(s, "secret") {
		return nil
	}
	return detectCredentialKeyCaptures(s, shellStdinSecretPattern, true, nil)
}

func detectCredentialValues(s string) []taggedRegion {
	// Authorization headers use ':', so they run before the '=' gate below.
	regions := detectAuthorizationHeaders(s)
	if !strings.ContainsRune(s, '=') {
		return regions
	}
	forEachUncoveredMatch(s, credentialValuePattern, 2, func(loc []int, wholeWord bool) int {
		// Main's shared placeholder rules, except that a single-quoted value
		// is a shell literal and only the literal placeholder words apply.
		valueRegion := credentialValueRegion
		if !wholeWord {
			valueRegion = capturedValueRegion
		}
		value, isReal := valueRegion(s, loc[4], loc[5], true, isRealPasswordPiece)
		if !isReal {
			return 0
		}
		regions = append(regions, taggedRegion{region: value})
		return value.end
	})
	if strings.Contains(s, "sig=") {
		for _, loc := range sasSignaturePattern.FindAllStringSubmatchIndex(s, -1) {
			regions = append(regions, taggedRegion{region: region{loc[2], loc[3]}})
		}
	}
	regions = append(regions, detectCredentialKeyCaptures(s, credentialAssignmentPattern, false, shouldPreserveCredentialCodeAssignment)...)
	return regions
}

func detectCredentialKeyCaptures(
	s string,
	pattern *regexp.Regexp,
	genericPasswordContext bool,
	shouldPreserve func(value, separator string) bool,
) []taggedRegion {
	valueGroup := pattern.SubexpIndex("value")
	keyGroup := pattern.SubexpIndex("key")
	var regions []taggedRegion
	visit := func(match []int, wholeWord bool) int {
		keyStart, keyEnd, ok := captureRange(match, keyGroup)
		if !ok {
			return 0
		}
		key := s[keyStart:keyEnd]
		credentialContext := genericPasswordContext || key == "PASSWORD" || key == "PASSWD"
		if genericPasswordContext {
			// A literal piped into a secrets-store write is a secret whatever
			// the key is called (STRIPE_KEY, DATABASE_URL); only a pure
			// identifier key such as CLIENT_ID names a non-secret.
			if isPureIdentifierKey(key) {
				return 0
			}
		} else if !isCredentialJSONSecretKey(key, credentialContext) {
			return 0
		}
		valueStart, valueEnd, _ := captureRange(match, valueGroup)
		judge := func(valueEnd int, wholeWord bool) (taggedRegion, bool) {
			return assignedValueCapture(s, key, keyEnd, valueStart, valueEnd, wholeWord, shouldPreserve)
		}
		// The whole shell word is the value ("None"x is not None).
		end := valueEnd
		_, wordEnd, joined := joinedShellWord(s, valueStart, valueEnd)
		if joined = joined && wholeWord; joined {
			end = wordEnd
		}
		captured, ok := judge(end, wholeWord)
		if joined && shouldPreserve != nil && readsPastJoinedWord(s, valueStart, valueEnd, wordEnd) {
			// Main reads the value past its word, so what either reading
			// redacts is redacted (see readsPastJoinedWord).
			main, mainOK := judge(valueEnd, false)
			captured.region, ok = eitherReading(captured.region, ok, main.region, mainOK)
		}
		if !ok {
			return 0
		}
		regions = append(regions, captured)
		return captured.end
	}
	forEachUncoveredMatch(s, pattern, valueGroup, visit)
	if shouldPreserve == nil {
		// Each shell-stdin literal is bounded by its own pipe and key, and a
		// word redacted whole can run into the next command's printf
		// (printf a"|... ; printf "x y"|...), where the search no longer
		// starts. So each match is also read as main reads it, as captured.
		// These literals are never extended past separators, so the reads
		// stay linear.
		for _, match := range pattern.FindAllStringSubmatchIndex(s, -1) {
			visit(match, false)
		}
	}
	return regions
}

// assignedValueCapture returns the region to redact for the value at
// s[valueStart:valueEnd] that key, which ends at keyEnd, is assigned, when it
// is a real credential. wholeWord reads it as a shell word joined from
// pieces (see nonPlaceholderCapture). shouldPreserve, nil for shell-stdin
// literals, recognizes code assignments.
func assignedValueCapture(s, key string, keyEnd, valueStart, valueEnd int, wholeWord bool, shouldPreserve func(value, separator string) bool) (taggedRegion, bool) {
	if isNonPasswordCodeLiteral(key, strings.Trim(s[valueStart:valueEnd], `"'`)) {
		return taggedRegion{}, false
	}
	if shouldPreserve != nil && keyEnd <= valueStart && shouldPreserve(s[valueStart:valueEnd], s[keyEnd:valueStart]) {
		return taggedRegion{}, false
	}
	captured, ok := nonPlaceholderCapture(s, key, valueStart, valueEnd, wholeWord, shouldPreserve != nil)
	// Assignments get the same separator handling as the password pattern:
	// "ab;cd" is one value. Shell-stdin literals stop at the pipe syntax
	// their pattern already bounds. A word that starts with a quoted string
	// ends where its quotes say; a quote that never closes ("abc;hunter2) is
	// one more byte of the value.
	if ok && shouldPreserve != nil && captured.start == valueStart && !startsWithQuotedString(s, valueStart) {
		captured.end = extendPastValueSeparators(s, captured.end)
	}
	return captured, ok
}

// forEachUncoveredMatch calls visit for each match of pattern in s, left to
// right, that has a value capture (group valueGroup). visit returns where the
// region it redacted ends, or 0 when it redacted nothing. wholeWord tells it
// to read the value's whole shell word; otherwise the value is read as main
// reads it, as the pattern captured it.
//
// A shell word redacted whole can hold more key=value text, as in
// DB_PASSWORD=a" DB_PASSWORD=b", and reading a value from inside the word
// would misread the rest of the line: a closing quote in the word would open
// a new string and run into a later password. So each search starts where the
// last match or redacted region ends, whichever is later, and finds what
// FindAll would find from there (see nextMatch). A later assignment after the
// word (b";DB_PASSWORD=hunter2) is still found, and as with FindAll no search
// starts before the last one's match ended, so a line of such words
// (DB_PASSWORD=a" "DB_PASSWORD=a" "...) takes linear time. The one exception
// is a region that ends in a key whose value comes after it, as when a value
// runs past a separator into db_password=foo,client_secret = "x": the next
// search starts at that key, which is read once more, so its value is found.
// That region is the one that ends last, which can be one of the regions
// visitStraddlingValues adds. The key can also be the value this match
// captured, as the second DB_PASSWORD in DB_PASSWORD = DB_PASSWORD = hunter2:
// the search then starts again inside the match, at a key that begins after
// the match's own key, so it moves on, and only the key is read again, since
// its value comes after the region. A key that begins with '_' right where the
// match ends is read as FindAll reads it, with the '_' as the byte before it,
// so API_KEY="x"_PASSWORD=+hunter2 has the key PASSWORD, as on main. Any other
// trailing key is read whole, and when its value stays visible and the key is
// in text FindAll reads, also as FindAll reads it when it goes on at the key's
// first byte, so API_KEY="x"x_PASSWORD=+hunter2 has the key PASSWORD as well
// (see visitCursorMatch). When the key's value is not redacted, as for b in
// DB_PASSWORD="a"b= API_KEY=hunter2, the search starts again where the region
// ends, so that value can not hide an assignment after the region. FindAll
// took that match, though, so a match found inside it whose value stays
// visible and runs past it does not take the text after it: the search goes
// on where that match ends, as FindAll does. In
// API_KEY="x"=a=\b= API_KEY=hunter2, b= is not an assignment whose
// value takes API_KEY=hunter2, while a match that ends inside it, as the
// empty API_KEY="" in API_KEY="a"b= API_KEY=""|API_KEY=hunter2xyz, moves the
// search on only to its own end. That holds only for a key that starts at or
// after the end of the region's match, in text FindAll goes on to read, and
// that FindAll can match there. A key inside the text that match took, as abc
// in DB_PASSWORD=abc= DB_USER= DB_NAME= API_KEY=hunter2, is one FindAll never
// reads, so its match sets no such end, and the search goes on from the
// region's end, where it finds DB_USER= DB_NAME= and then API_KEY=hunter2, as
// FindAll does. So is FOO in API_KEY="x"FOO= DB_USER= DB_NAME= API_KEY=hunter2,
// read whole with the match's closing quote before it, which FindAll can not
// take as the byte before a key (see endsRejectedMatch). Nor, when its
// value is redacted, does it move where FindAll goes on: in API_KEY=API_KEY=
// a= b= API_KEY=hunter2xyz, the second API_KEY takes a= as its value, but
// FindAll's match a= b= still starts at the first match's end, so b= does not
// take API_KEY=hunter2xyz.
//
// The only values read from inside a region are those visitStraddlingValues
// finds, and they add to what the region already redacts. One that stays
// visible, as a="+" in api_key="x""a="+"b=";client_secret="hunter2", was
// still matched, so as with FindAll no match takes the last byte of its match
// as the byte before a key: the closing quote there would make
// ";client_secret=" the value of b and hide the client_secret. The same holds
// for one whose value is redacted short of its match's end, as API_KEY="x'"
// in API_KEY=""'API_KEY="x'"a= API_KEY=hunter2, whose value keeps its quotes:
// its closing quote would make a= API_KEY=hunter2 an assignment. A match that
// does take that byte is still read, and kept when its value is redacted,
// which only redacts more than FindAll does, as API_KEY after the closing
// quote of "a" in API_KEY="x""|api_key="a"API_KEY=hunter2xyz. This holds
// only for a match FindAll finds, going on from the end of the match the
// region was redacted for (see findAllChain). One that starts before that
// end, inside the text that match took, as b='b' in
// API_KEY='b='b'API_KEY=hunter2xyz, is one FindAll never finds, so the
// second API_KEY is still read. So is one inside a match FindAll finds after
// that end: in api_key="x""a=b="; note=" f=";client_secret="hunter2", FindAll
// takes a=b=" whole, so b="; note=" sets no such end, and note=" f=" and then
// client_secret are read as FindAll reads them. A later
// match that starts inside such a match and whose value stays visible and
// runs past it is cut back to its end, as for a rejected trailing key, so
// FOO= in API_KEY="x""a=b"|FOO= API_KEY=hunter2, which a's value holds, does
// not take API_KEY=hunter2.
func forEachUncoveredMatch(s string, pattern *regexp.Regexp, valueGroup int, visit func(match []int, wholeWord bool) int) {
	covered, atKey, unredactedEnd, rejectedEnd := 0, false, -1, -1
	// trailing marks a search that starts again at a key that ends a
	// redacted region.
	trailing := false
	// coveredMatchEnd is where the match that covered was redacted for ends,
	// where FindAll goes on.
	coveredMatchEnd := 0
	// findAllKeyAt is where that trailing key starts, to read it as FindAll
	// does too (see visitCursorMatch), or -1.
	findAllKeyAt := -1
	runs := valueRunEnd{start: -1, end: -1}
	for from := 0; from < len(s); {
		searchFrom := from
		if atKey {
			// The byte before the key is not a key byte; FindAll matches
			// the key with it.
			searchFrom--
		}
		match := cursorMatch(s, pattern, valueGroup, searchFrom, from, trailing, &runs)
		keyAt := findAllKeyAt
		trailing, findAllKeyAt = false, -1
		if match == nil {
			return
		}
		next := max(match[1], from+1)
		atKey = false
		// readByte marks a match whose key follows the last byte of a match
		// that was read, which FindAll would not match the key with.
		readByte := match[0] == unredactedEnd-1
		if valueStart, _, ok := captureRange(match, valueGroup); ok {
			if valueStart < covered {
				next = afterCovered(readByte, covered, unredactedEnd)
			} else if read, end := visitCursorMatch(s, pattern, valueGroup, visit, match, keyAt, &runs); end > 0 {
				match = read
				coveredMatchEnd = findAllGoesOn(from, coveredMatchEnd, match[1])
				last, readEnd := visitStraddlingValues(s, pattern, valueGroup, visit, match, end, coveredMatchEnd, &runs)
				covered, unredactedEnd = max(covered, last.end), max(unredactedEnd, readEnd)
				if keyStart := assignedKeyStart(s, last.start, last.end); keyStart >= valueStart && last.end >= next {
					// FindAll goes on at match[1], so it reads a '_' there as
					// the byte before the key.
					next, atKey, trailing = keyStart, keyStart != match[1] || s[keyStart] != '_', true
					findAllKeyAt = keyFindAllReads(keyStart, coveredMatchEnd)
				} else if keyStart < 0 {
					next = max(next, last.end)
				}
			} else if match[0] < covered || readByte {
				next = afterCovered(readByte, covered, unredactedEnd)
				if !readByte && endsRejectedMatch(from, match[0], valueStart, covered, coveredMatchEnd) {
					rejectedEnd = match[1]
				}
			} else {
				next = endAtFindAllBoundary(match[0], next, from, rejectedEnd, unredactedEnd)
			}
		}
		from = next
	}
}

// afterCovered returns where the search goes on after a match it skips: one
// whose value, or whose start, is inside the text the regions so far cover,
// which ends at covered, or one whose value stays visible and whose key
// follows the last byte of a match that was read. When the key follows that
// byte, the search goes on at readEnd, where that match ends, as FindAll
// would, and otherwise at covered.
func afterCovered(readByte bool, covered, readEnd int) int {
	if readByte {
		return readEnd
	}
	return covered
}

// endsRejectedMatch reports whether a match that starts at matchStart, read
// from from, whose value stays visible and starts at valueStart, after a
// region that ends at regionEnd, ends the matches the search finds inside
// it, as for one FindAll took, where FindAll goes on at findAllEnd. A match
// read again inside the text FindAll's match took is not one of them. Nor is
// a key read with the last byte of that match before it, which FindAll can
// not take as the byte before a key, as FOO in
// API_KEY="x"FOO= DB_USER= DB_NAME= API_KEY=hunter2: its end would cut
// DB_USER= DB_NAME= short, and DB_NAME would then take API_KEY=hunter2 as its
// value. Such a key whose value starts right where the region ends still
// counts, as a in API_KEY="x"a=\b= API_KEY=hunter2, where the word ends at
// the backslash and its value is \b=, so b= does not take API_KEY=hunter2
// either. FOO's value starts only after the blank that ends the word, and in
// the shell FOO has none.
func endsRejectedMatch(from, matchStart, valueStart, regionEnd, findAllEnd int) bool {
	return from >= findAllEnd && (matchStart >= findAllEnd || valueStart == regionEnd)
}

// visitCursorMatch calls visit for match and returns match and what visit
// returned. When match reads the trailing key at findAllKeyAt whole and its
// value stays visible, it also reads the key as FindAll does when FindAll goes
// on at its first byte, after the next byte that can come before a key, and
// returns that reading when its value is redacted. In
// API_KEY="a"x_PASSWORD=+hunter2xyz, the word ends at the +, so the search
// starts again at x_PASSWORD, which is not a credential key, but FindAll goes
// on at the x and reads PASSWORD, which is. In
// api_key="a"api_key="a"_api_key= hunter2xyz, FindAll's match _key="a" ends at
// the last '_', so FindAll reads api_key, not _api_key. Both readings share
// the value, so it is redacted when either key is a credential's. Where
// FindAll goes on before the key, it reads the key whole, so the second
// reading can only redact more than main does.
func visitCursorMatch(s string, pattern *regexp.Regexp, valueGroup int, visit func(match []int, wholeWord bool) int, match []int, findAllKeyAt int, runs *valueRunEnd) ([]int, int) {
	end := visit(match, true)
	if end > 0 || findAllKeyAt < 0 {
		return match, end
	}
	read := trailingKeyMatch(s, pattern, valueGroup, findAllKeyAt, findAllKeyAt, runs)
	if read == nil || read[0] == match[0] || read[2*valueGroup] != match[2*valueGroup] {
		return match, end
	}
	if readEnd := visit(read, true); readEnd > 0 {
		return read, readEnd
	}
	return match, end
}

// keyFindAllReads returns keyStart, where a trailing key starts, when it is in
// text FindAll reads, going on at findAllEnd, and -1 when it is inside the text
// a match FindAll took, which FindAll never reads, as xDB_PASSWORD in
// api_key=xDB_PASSWORD= x.
func keyFindAllReads(keyStart, findAllEnd int) int {
	if keyStart < findAllEnd {
		return -1
	}
	return keyStart
}

// cursorMatch returns the match forEachUncoveredMatch reads from searchFrom:
// for a search that starts again at the trailing key at from, the bounded
// trailingKeyMatch, and otherwise nextMatch.
func cursorMatch(s string, pattern *regexp.Regexp, valueGroup, searchFrom, from int, trailing bool, runs *valueRunEnd) []int {
	if trailing {
		return trailingKeyMatch(s, pattern, valueGroup, searchFrom, from, runs)
	}
	return nextMatch(s, pattern, searchFrom, len(s))
}

// findAllGoesOn returns where FindAll goes on after the match from a search
// at from that ends at matchEnd, when it went on at findAllEnd before. A key
// inside the text the last match took, read again because it ends a redacted
// region, is one FindAll never reads, so FindAll still goes on at findAllEnd.
func findAllGoesOn(from, findAllEnd, matchEnd int) int {
	if from < findAllEnd {
		return findAllEnd
	}
	return matchEnd
}

// endAtFindAllBoundary returns where the search goes on after a match from
// start to next whose value stays visible: at the first of ends, the ends of
// matches FindAll took, that the match starts before and runs past, as FindAll
// goes on there, and otherwise at next. It moves on by at least a byte past
// from.
func endAtFindAllBoundary(start, next, from int, ends ...int) int {
	for _, end := range ends {
		if start < end && end < next {
			next = max(end, from+1)
		}
	}
	return next
}

// nextMatch returns the first match of pattern in s[from:limit] that FindAll
// would find after a match that ended at from, with its indexes on s. The
// key/value patterns open with (?:^|[^A-Za-z0-9]): a key follows the start of
// the text or a byte that is not a letter or digit, and that byte is part of
// the match. Searching s[from:] would read ^ at from, so for from > 0 a key
// could begin right there, where FindAll finds none because the byte before
// from belongs to text already read: b in FOO="x"b= API_KEY=hunter2 would
// take API_KEY=hunter2 as its value. So a search that would start at a letter,
// digit or '_' starts instead just past the first byte from there on that can
// come before a key, where ^ stands for that byte. The shell-stdin pattern opens
// with \bprintf, which also needs a byte before it that is not a letter or
// digit, so the same start finds what FindAll would, or more.
func nextMatch(s string, pattern *regexp.Regexp, from, limit int) []int {
	if from > 0 && from < limit && (isASCIIAlnum(s[from]) || s[from] == '_') {
		for from < limit && isASCIIAlnum(s[from]) {
			from++
		}
		if from >= limit {
			return nil
		}
		from++
	}
	match := pattern.FindStringSubmatchIndex(s[from:limit])
	if match != nil {
		offsetMatch(match, from)
	}
	return match
}

// visitStraddlingValues calls visit for each value that starts inside the
// region s[match's value start:end], which visit redacted for match, and runs
// past it, does the same for the regions those values are redacted in, and
// returns the region that ends last, which is the first unless one of theirs
// ends past it. There are two kinds:
//
//   - A key's quoted value that opens inside the region and closes past it,
//     as API_KEY in abc\" API_KEY="hunter2 x"tail, where the shell word ends
//     at the blank. That reads the line as if the region's own quote there
//     were escaped, so the value's shell word ("hunter2 x"tail) is redacted
//     whole, as any other. Its opening quote is the last quote of its kind in
//     the region, so a region has at most two, and the pattern is matched only
//     over the key and its quoted string.
//   - A bare value that FindAll, searching from the region's start or from
//     where its match ends, as main does, reads past the region's end, as
//     API_KEY in abc\" API_KEY=aaa\"|bbb, where the word ends at the pipe. It
//     is read as main reads it, as captured: read on as a shell word from
//     inside the region, its quotes could pair with the region's own.
//
// Two regions can share a value, so each is read once, and a value is searched
// in turn only when its region ends past the one it was found in: one that
// ends inside it adds nothing, and searching it would read a line such as
// DB_PASSWORD=DB_PASSWORD=...hunter2, once for each key. runs finds where the
// run of value bytes at a region's end ends (see valueRunEnd). It also returns
// where the last match it read whose value stays visible, or is redacted short
// of the match's end, ends, of those that FindAll finds going on at
// findAllFrom, or -1, since FindAll would go on from there (see
// forEachUncoveredMatch).
func visitStraddlingValues(s string, pattern *regexp.Regexp, valueGroup int, visit func(match []int, wholeWord bool) int, match []int, end, findAllFrom int, runs *valueRunEnd) (region, int) {
	type straddled struct {
		r        region
		matchEnd int
	}
	last, matchEnd := region{match[2*valueGroup], end}, match[1]
	pending := []straddled{{last, matchEnd}}
	read := map[[2]int]bool{}
	unredactedEnd := -1
	findAll := findAllChain{from: findAllFrom, starts: map[int]bool{}}
	readValue := func(match []int, wholeWord bool, inside region) {
		valueEnd := visit(match, wholeWord)
		if valueEnd < match[1] && findAll.holds(s, pattern, valueGroup, match[2*valueGroup], runs) {
			unredactedEnd = max(unredactedEnd, match[1])
		}
		if valueEnd <= 0 {
			return
		}
		value := region{match[2*valueGroup], valueEnd}
		if value.end > last.end {
			last = value
		}
		if value.end > inside.end {
			pending = append(pending, straddled{value, match[1]})
		}
	}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, quote := range []byte{'"', '\''} {
			if match := quotedValuePast(s, pattern, valueGroup, current.r, quote); match != nil && !read[matchKey(match, valueGroup)] {
				read[matchKey(match, valueGroup)] = true
				readValue(match, true, current.r)
			}
		}
		for _, match := range bareValuesPast(s, pattern, valueGroup, current.r, current.matchEnd, runs) {
			if !read[matchKey(match, valueGroup)] {
				read[matchKey(match, valueGroup)] = true
				readValue(match, false, current.r)
			}
		}
	}
	return last, unredactedEnd
}

// matchKey identifies a match by where it starts and where its value does.
// The searches from a region's start and from its match's end can read one
// value under two keys, as _api_key and api_key in "x"_api_key=b=\hunter2xyz,
// and the second, FindAll's, is read too when the first is not a credential.
func matchKey(match []int, valueGroup int) [2]int {
	return [2]int{match[0], match[2*valueGroup]}
}

// quotedValuePast returns the match for the key whose value, quoted with
// quote, opens inside r and closes past it, or nil. The value closes where the
// pattern reads it closing, which for DB_PASSWORD= can be on a later line
// (DB_PASSWORD="hunter2\nsecretpart"), while the shell word that holds r
// ends with its line.
func quotedValuePast(s string, pattern *regexp.Regexp, valueGroup int, r region, quote byte) []int {
	open := strings.LastIndexByte(s[r.start:r.end], quote)
	if open < 0 {
		return nil
	}
	open += r.start
	keyStart := assignmentKeyStart(s, r.start, open)
	if keyStart < 0 {
		return nil
	}
	closeAt := strings.IndexByte(s[open+1:], quote)
	if closeAt < 0 {
		return nil
	}
	if closeAt += open + 1; closeAt <= r.end {
		return nil
	}
	match := pattern.FindStringSubmatchIndex(s[keyStart : closeAt+1])
	if match == nil {
		return nil
	}
	offsetMatch(match, keyStart)
	if valueStart, valueEnd, ok := captureRange(match, valueGroup); !ok || valueStart != open || valueEnd != closeAt+1 {
		return nil
	}
	return match
}

// bareValuesPast returns the matches with a value that starts inside r and
// runs past its end, found by searching as FindAll does from two places: from
// r's start, which reads the keys inside r's value, as DB_PASSWORD in
// "x DB_PASSWORD=abc\";hunter2", and from matchEnd, where the match r was
// redacted for ends and main's FindAll goes on, as for API_KEY in
// a=" API_KEY=aaa\"|bbb", where the search from r's start reads a= as a key
// and takes the quoted text as its value. Such a value holds the byte at
// r.end, so the searches stop where that run of value bytes does. A value
// that a separator ends at or before r.end is returned too when the text the
// separator joins to it runs past r.end, because it is judged with that text,
// as in API_KEY=a"b;c"x after a word that ended at the ';', and API_KEY in
// DB_PASSWORD="a\"b";API_KEY=\;"a"hunter2xyz, where the word read from the
// first quote ends at the last one.
func bareValuesPast(s string, pattern *regexp.Regexp, valueGroup int, r region, matchEnd int, runs *valueRunEnd) [][]int {
	limit := r.end
	if limit >= len(s) || strings.IndexByte(",;&", s[limit]) < 0 {
		if limit = runs.from(s, r.end); limit == r.end {
			return nil
		}
	}
	var matches [][]int
	for _, from := range []int{r.start, matchEnd} {
		for from < r.end {
			match := nextValueMatch(s, pattern, valueGroup, from, r.end, limit)
			if match == nil {
				break
			}
			valueStart, valueEnd, ok := captureRange(match, valueGroup)
			if ok && valueStart < r.end && (valueEnd >= r.end || extendPastValueSeparators(s, valueEnd) > r.end) {
				matches = append(matches, match)
			}
			from = max(match[1], from+1)
		}
	}
	return matches
}

// valueHiddenText is the longest fixed value credentialAssignmentPattern
// reads, (value hidden).
const valueHiddenText = "(value hidden)"

// nextValueMatch returns nextMatch(s, pattern, from, limit) for a search that
// only needs matches whose value starts before end, where limit ends the run
// of value bytes that holds the byte at end. A bare value that holds that byte
// runs on to limit, so on a long line without blanks, searching on to limit
// for each region would read the rest of the line once per region. So the
// search reads only the text up to len(valueHiddenText) bytes past end, which
// holds any (value hidden) that starts before end. A value that reaches the
// end of that window and doesn't open with a quote is a bare value of the key
// and password patterns, which stop at the bytes that end the run, so it ends
// at limit. A value that opens with a quote can close past the window, so that
// match is found again over the whole text: its closing quote, or the end of
// its run when it has none, is the first one after it, so these searches don't
// read the same text twice. The shell-stdin pattern goes on past its value, to
// the command it pipes into, and bounds its literal, so it is searched as
// before.
func nextValueMatch(s string, pattern *regexp.Regexp, valueGroup, from, end, limit int) []int {
	window := min(limit, end+len(valueHiddenText))
	if pattern == shellStdinSecretPattern || window == limit {
		return nextMatch(s, pattern, from, limit)
	}
	match := nextMatch(s, pattern, from, window)
	valueStart, valueEnd, ok := captureRange(match, valueGroup)
	switch {
	case !ok || valueStart >= end:
		return match
	case s[valueStart] == '"' || s[valueStart] == '\'':
		return nextMatch(s, pattern, from, limit)
	case valueEnd == window && match[1] == valueEnd:
		match[2*valueGroup+1], match[1] = limit, limit
	}
	return match
}

// findAllValueMatch is nextValueMatch for a search with no run end known,
// whose match must end where FindAll's does: a bare value that runs past the
// window ends where its run of value bytes does, and one that opens with a
// quote is found again over the whole text.
func findAllValueMatch(s string, pattern *regexp.Regexp, valueGroup, from, end int, runs *valueRunEnd) []int {
	match := nextValueMatch(s, pattern, valueGroup, from, end, len(s))
	valueStart, valueEnd, ok := captureRange(match, valueGroup)
	if ok && valueStart < end && valueEnd == len(s) && !isQuoteByte(s[valueStart]) && pattern != shellStdinSecretPattern {
		runEnd := runs.from(s, valueStart)
		match[2*valueGroup+1], match[1] = runEnd, runEnd
	}
	return match
}

// trailingKeyMatch is nextMatch(s, pattern, from, len(s)) for the search
// that starts again at a key that ends a redacted region (see
// forEachUncoveredMatch). That key's bare value can run on to the end of a
// line without blanks, as FOO's does in API_KEY="x"FOO=|API_KEY="x"FOO=|...,
// and reading it for each word would read the rest of the line once per word.
// So the search reads only a few bytes past where the value starts, and the
// value ends where its run of value bytes does (see findAllValueMatch).
func trailingKeyMatch(s string, pattern *regexp.Regexp, valueGroup, from, keyStart int, runs *valueRunEnd) []int {
	if eq := strings.IndexByte(s[keyStart:], '='); eq >= 0 {
		valueStart := len(s) - len(strings.TrimLeft(s[keyStart+eq+1:], " \t"))
		match := findAllValueMatch(s, pattern, valueGroup, from, valueStart+1, runs)
		if start, _, ok := captureRange(match, valueGroup); ok && start == valueStart {
			return match
		}
	}
	return nextMatch(s, pattern, from, len(s))
}

// findAllChain follows the matches FindAll finds after it goes on at from, as
// far as it needs to tell whether one of them has a given value start.
type findAllChain struct {
	from int
	// starts holds the value starts of the matches found so far; every match
	// with a value start before reached has been found.
	starts  map[int]bool
	reached int
}

// holds reports whether the value that starts at valueStart is one of the
// matches FindAll finds.
func (c *findAllChain) holds(s string, pattern *regexp.Regexp, valueGroup, valueStart int, runs *valueRunEnd) bool {
	for c.reached <= valueStart && c.from <= valueStart {
		match := findAllValueMatch(s, pattern, valueGroup, c.from, valueStart+1, runs)
		start, _, ok := captureRange(match, valueGroup)
		if !ok || start > valueStart {
			c.reached = valueStart + 1
			break
		}
		c.starts[start], c.reached = true, start+1
		c.from = max(match[1], c.from+1)
	}
	return c.starts[valueStart]
}

// valueRunStopBytes end a run of bare value bytes: blanks, line ends and the
// value separators.
const valueRunStopBytes = " \t\n\f\r,;&"

// valueRunEnd finds where a run of bare value bytes ends. It remembers the
// run it found furthest along the text, s[start:end], which holds no stop
// byte, so the regions on a long line without blanks, as in
// DB_PASSWORD="a"|DB_PASSWORD="a"|..., which all end inside one run, share
// one scan of it instead of each scanning on to its end.
type valueRunEnd struct {
	start, end int
}

// from returns the index of the first of valueRunStopBytes at or after s[i],
// or len(s).
func (runs *valueRunEnd) from(s string, i int) int {
	if runs.start <= i && i <= runs.end {
		return runs.end
	}
	end := i
	for end < len(s) && strings.IndexByte(valueRunStopBytes, s[end]) < 0 {
		if end == runs.start {
			// The rest of the run is the one remembered.
			runs.start = i
			return runs.end
		}
		end++
	}
	if i > runs.end {
		runs.start, runs.end = i, end
	}
	return end
}

// offsetMatch moves the indexes of a match made on s[from:] onto s.
func offsetMatch(match []int, from int) {
	for i := range match {
		if match[i] >= 0 {
			match[i] += from
		}
	}
}

// assignedKeyStart returns where the key at the end of the redacted region
// s[start:end] begins when an = assigns it a value that starts after end, or
// -1.
func assignedKeyStart(s string, start, end int) int {
	if s[end-1] == '=' {
		return assignmentKeyStart(s, start, end)
	}
	next := end
	for next < len(s) && (s[next] == ' ' || s[next] == '\t') {
		next++
	}
	if next < len(s) && s[next] == '=' {
		if keyStart := keyStartBefore(s, start, end); keyStart < end {
			return keyStart
		}
	}
	return -1
}

// assignmentKeyStart returns where the key begins in a key= or key = that
// ends at s[at], all inside s[lo:at], or -1 when s[lo:at] does not end so.
func assignmentKeyStart(s string, lo, at int) int {
	j := at
	for j > lo && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	if j == lo || s[j-1] != '=' {
		return -1
	}
	j--
	for j > lo && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	if keyStart := keyStartBefore(s, lo, j); keyStart < j {
		return keyStart
	}
	return -1
}

// keyStartBefore returns where the run of key bytes that ends at s[end]
// begins, no earlier than lo; end when there is none.
func keyStartBefore(s string, lo, end int) int {
	keyStart := end
	for keyStart > lo && isCredentialKeyByte(s[keyStart-1]) {
		keyStart--
	}
	return keyStart
}

// isCredentialKeyByte reports whether c can be part of a key that the
// key/value patterns read ([A-Za-z0-9_.-]).
func isCredentialKeyByte(c byte) bool {
	return isASCIIAlnum(c) || c == '_' || c == '.' || c == '-'
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

// shouldPreserveCredentialCodeAssignment recognizes source expressions that
// cannot be credential literals. Requiring whitespace on both sides of `=`
// keeps compact shell/env assignments on the conservative redaction path.
func shouldPreserveCredentialCodeAssignment(value, separator string) bool {
	equal := strings.IndexByte(separator, '=')
	if equal <= 0 || equal >= len(separator)-1 {
		return false
	}
	return value == "await" || credentialCodeCallPattern.MatchString(value) || isCredentialCodeReference(value)
}

func isCredentialCodeReference(value string) bool {
	if !credentialCodeRefPattern.MatchString(value) {
		return false
	}
	memberStart := strings.LastIndexByte(value, '.') + 1
	return isCredentialJSONSecretKey(value[memberStart:], false)
}

// nonPlaceholderCapture returns the value captured at s[start:end] under key
// when it is a real credential. wholeWord reads it as a shell word joined
// from pieces; otherwise it is read as captured. extendJoined judges a joined
// word that starts as bare text after the separator extension its region gets
// anyway, as the password pattern does, so ${A}"${B}",hunter2 is one value.
func nonPlaceholderCapture(s, key string, start, end int, wholeWord, extendJoined bool) (taggedRegion, bool) {
	isReal := func(piece string, singleQuoted bool) bool {
		return hasNonPlaceholderKeyValue(key, piece, singleQuoted)
	}
	value, secret, joined := region{}, false, false
	if wholeWord {
		value, secret, joined = joinedValueRegion(s, start, end, extendJoined, isReal)
	}
	if !joined {
		value, secret = capturedValueRegion(s, start, end, false, isReal)
	}
	if !secret {
		return taggedRegion{}, false
	}
	return taggedRegion{region: value}, true
}

// isRealPasswordPiece applies the shared placeholder rules to a password
// value, where single quotes leave only the literal placeholder words.
func isRealPasswordPiece(value string, singleQuoted bool) bool {
	if singleQuoted {
		return hasNonPlaceholderQuotedPasswordValue(value, true)
	}
	return hasNonPlaceholderPasswordValue(value)
}

// credentialValueRegion returns the region to redact for the value a
// key/value pattern captured at s[start:end], and whether it holds a real
// secret. isReal judges one piece of the value: its text, and whether single
// quotes made that text a literal. extend continues an unquoted value past
// value separators before it is judged, so "changeme;hunter2" is one value.
// A value that is one quoted string is redacted inside its quotes, and a
// shell word joined from several pieces (""'$X', "hunter"'2') is redacted
// whole and judged by isRealJoinedWord. A word that starts with a quoted
// string was quoted on purpose, so a separator after it ends a command, not
// the value ("x"'y';echo).
func credentialValueRegion(s string, start, end int, extend bool, isReal func(piece string, singleQuoted bool) bool) (region, bool) {
	if value, secret, joined := joinedValueRegion(s, start, end, extend, isReal); joined {
		return value, secret
	}
	return capturedValueRegion(s, start, end, extend, isReal)
}

// joinedValueRegion is credentialValueRegion for a value whose shell word is
// joined from several pieces, and joined is false for any other value.
func joinedValueRegion(s string, start, end int, extend bool, isReal func(piece string, singleQuoted bool) bool) (value region, secret, joined bool) {
	pieces, wordEnd, joined := joinedShellWord(s, start, end)
	if !joined || extend && readsPastJoinedWord(s, start, end, wordEnd) {
		return region{}, false, false
	}
	if extend && !isQuoteByte(s[start]) {
		if extended := extendPastValueSeparators(s, wordEnd); extended > wordEnd {
			pieces = append(pieces, wordPiece{start: wordEnd, end: extended})
			wordEnd = extended
		}
	}
	return region{start, wordEnd}, isRealJoinedWord(s, pieces, isReal), true
}

// readsPastJoinedWord reports whether main's reading of a bare value captured
// at s[start:end], extended past value separators, runs past the shell word
// that ends at wordEnd, extended the same way. The pattern can stop at a
// separator inside a quoted piece of the word, and the word can then end at
// a quote main does not pair: the shell reads a"b;c\"d|hunter2xyz" as one
// quoted string, but a backslash before a quote is left alone (see
// escapedWordBytes), so the word ends at the pipe. Such a value is judged
// both as its word and as main reads it, and what either reading redacts is
// redacted, so it never redacts less than main does, nor less than its word:
// main judges changeme" alone in changeme";hunter2"|cat, a placeholder.
func readsPastJoinedWord(s string, start, end, wordEnd int) bool {
	return !isQuoteByte(s[start]) && extendPastValueSeparators(s, end) > extendPastValueSeparators(s, wordEnd)
}

// eitherReading returns what a value's word reading and main's reading of it
// redact together: both regions start at the value, so when both readings
// find a secret one region covers them, and otherwise the one that does.
func eitherReading(word region, wordSecret bool, main region, mainSecret bool) (region, bool) {
	switch {
	case wordSecret && mainSecret:
		return region{min(word.start, main.start), max(word.end, main.end)}, true
	case wordSecret:
		return word, true
	default:
		return main, mainSecret
	}
}

// capturedValueRegion is credentialValueRegion for a value read as main reads
// it, as the pattern captured it: one quoted string is redacted inside its
// quotes, and a bare value is extended past separators when extend is set.
func capturedValueRegion(s string, start, end int, extend bool, isReal func(piece string, singleQuoted bool) bool) (region, bool) {
	valueStart, valueEnd, singleQuoted := unquoteRangeQuoted(s, start, end)
	if extend && valueStart == start {
		valueEnd = extendPastValueSeparators(s, valueEnd)
	}
	value := s[valueStart:valueEnd]
	if valueStart > start && s[start] == '"' {
		value = doubleQuotedText(value)
	}
	return region{valueStart, valueEnd}, isReal(value, singleQuoted)
}

// isRealJoinedWord reports whether a shell word joined from pieces holds a
// real secret. Empty pieces hold nothing. A word with one piece of text is
// judged by that piece under its own quotes: ""'$X' is the literal $X, and
// ""'changeme' is a placeholder. The text of two or more pieces joins into a
// new literal (changeme'example' is changemeexample), so the word is a
// placeholder only when every piece is made of shell references that isReal
// accepts as placeholders ("$A"${B}, "$A"${B}${C}; see isReferenceRun).
func isRealJoinedWord(s string, pieces []wordPiece, isReal func(piece string, singleQuoted bool) bool) bool {
	var text []wordPiece
	for _, piece := range pieces {
		if piece.end > piece.start {
			text = append(text, piece)
		}
	}
	switch len(text) {
	case 0:
		return false
	case 1:
		piece := s[text[0].start:text[0].end]
		if text[0].quote == '"' {
			piece = doubleQuotedText(piece)
		}
		return isReal(piece, text[0].quote == '\'')
	}
	for _, piece := range text {
		if piece.quote == '\'' || !isReferenceRun(s[piece.start:piece.end], piece.quote == '"', isReal) {
			return true
		}
	}
	return false
}

// isReferenceRun reports whether text is a shell reference that isReal
// accepts as a placeholder, or a run of such references with nothing between
// them: $A, ${B} and ${B}${C}, but not ${B}x or ${B:-x}. doubleQuoted reads
// each reference as double quotes enclose it (see doubleQuotedText).
func isReferenceRun(text string, doubleQuoted bool, isReal func(piece string, singleQuoted bool) bool) bool {
	if text == "" || text[0] != '$' {
		return false
	}
	isPlaceholder := func(ref string) bool {
		if doubleQuoted {
			ref = doubleQuotedText(ref)
		}
		return !isReal(ref, false)
	}
	if isPlaceholder(text) {
		return true
	}
	for i := 0; i < len(text); {
		end := shellReferenceEnd(text, i)
		if end < 0 || !isPlaceholder(text[i:end]) {
			return false
		}
		i = end
	}
	return true
}

// shellReferenceEnd returns where the shell reference that starts at text[i]
// ends: a ${...} that closes, a $NAME, or a $ and one digit or special
// parameter. It returns -1 when text[i] does not start one.
func shellReferenceEnd(text string, i int) int {
	if text[i] != '$' || i+1 >= len(text) {
		return -1
	}
	switch c := text[i+1]; {
	case c == '{':
		if closeAt := wordBraceExpansionEnd(text, i+1); closeAt >= 0 {
			return closeAt + 1
		}
		return -1
	case '0' <= c && c <= '9' || strings.IndexByte("@*#?$!-", c) >= 0:
		return i + 2
	case c == '_' || isASCIIAlnum(c):
		end := i + 2
		for end < len(text) && (text[end] == '_' || isASCIIAlnum(text[end])) {
			end++
		}
		return end
	}
	return -1
}

func isQuoteByte(c byte) bool {
	return c == '"' || c == '\''
}

// wordPiece is one quoted string or run of bare text in a shell word. Its
// text is s[start:end], inside the quotes; quote is the quote byte, or 0.
type wordPiece struct {
	start, end int
	quote      byte
}

// joinedShellWord reads on through the shell word whose value a key/value
// pattern captured at s[start:end]. The shell joins quoted strings and bare
// text with nothing between them into one value, so ""'$X' is the literal $X
// and "hunter"'2' is hunter2. A pattern stops at the first closing quote, or,
// for a bare value, at a blank inside a later quoted string (abc"d e"). It
// returns the word's pieces and end, and false when the word is the capture
// itself as a single piece. Quoted strings close on their own line.
func joinedShellWord(s string, start, end int) ([]wordPiece, int, bool) {
	if start >= end {
		return nil, end, false
	}
	startsQuoted := isQuoteByte(s[start])
	if !startsQuoted && !strings.ContainsAny(s[start:end], `"'`) {
		return nil, end, false
	}
	var pieces []wordPiece
	i := start
	for i < len(s) {
		bareStart := i
		if c := s[i]; isQuoteByte(c) {
			if closeAt := quotedPieceEnd(s, i); closeAt >= 0 {
				pieces = append(pieces, wordPiece{start: i + 1, end: closeAt, quote: c})
				i = closeAt + 1
				continue
			}
			// A quote that never closes ends a word that started quoted;
			// in bare text it is one more byte, as the pattern read it.
			if startsQuoted {
				break
			}
			i++
		}
		bareEnd := bareWordTextEnd(s, i, startsQuoted)
		if bareEnd == bareStart {
			break
		}
		pieces = append(pieces, wordPiece{start: bareStart, end: bareEnd})
		i = bareEnd
	}
	if i < end || len(pieces) < 2 && i == end {
		return nil, end, false
	}
	markEnclosingString(s, pieces)
	return pieces, i, true
}

// markEnclosingString handles a word that ends in a quote that never closes,
// as the pattern read it: in echo "DB_PASSWORD=${PW}", the quote closes the
// string the word sits in. That quote holds no text of the value, and when it
// is a double quote, the bare text before it was inside double quotes, which
// isRealJoinedWord needs to read "${PW:-'$FALLBACK'}" (see doubleQuotedText).
func markEnclosingString(s string, pieces []wordPiece) {
	last := &pieces[len(pieces)-1]
	if last.quote != 0 || last.end-last.start != 1 || !isQuoteByte(s[last.start]) {
		return
	}
	last.start = last.end
	if s[last.end-1] != '"' {
		return
	}
	for i := range pieces {
		if pieces[i].quote == 0 {
			pieces[i].quote = '"'
		}
	}
}

// startsWithQuotedString reports whether a quoted string that closes on its
// line starts at s[i].
func startsWithQuotedString(s string, i int) bool {
	return isQuoteByte(s[i]) && quotedPieceEnd(s, i) >= 0
}

// quotedPieceEnd returns the index of the quote that closes the string opened
// at s[open], or -1 when the line ends first.
func quotedPieceEnd(s string, open int) int {
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case s[open]:
			return i
		case '\n', '\r':
			return -1
		}
	}
	return -1
}

// bareWordStopBytes end the bare text of any shell word: blanks, quotes,
// value separators (, ; &) and the other shell operators (| < > ( ) and a
// backtick), so printf v"x"|secrets keeps its pipe. An = is part of the word,
// as in the shell: DB_PASSWORD=abc"d e"=hunter2 is the value abcd e=hunter2.
const bareWordStopBytes = " \t\n\r\f\v\"',;&|<>()`"

// codeAfterStringBytes also end the bare text of a word that started as a
// quoted string. Right after a string they read as code or Markdown, not as
// more of the value: "x".strip(), cond ? "x":"y", "x"+y, **"x"**, "x"[0],
// and "x"\n in JSON-escaped text.
const codeAfterStringBytes = ".:+*?[]{}\\"

// escapedWordBytes are the bareWordStopBytes that a backslash makes part of
// a word that started as bare text, as the shell reads them: the | in
// a"b"\|c is text. Two backslashes are one literal backslash and escape
// nothing, so the blank in a"b"\\ echo still ends the word. A backslash
// before a quote is left as it is, because raw JSON text escapes quotes the
// same way, and one before a line end continues the line rather than the
// word.
const escapedWordBytes = " \t,;&|<>()`"

// bareWordTextEnd returns where the bare text of a shell word that starts at
// s[i] ends: at one of bareWordStopBytes, unless a backslash escapes it in a
// word that started as bare text (see escapedWordBytes). A ${...} expansion
// closed on its line is read whole, quotes and all, so ${PW:-""} and "$A"${B}
// are each one piece. In a word that started as a quoted string,
// codeAfterStringBytes, a $( command substitution and a ${ that does not
// close end it too.
func bareWordTextEnd(s string, i int, startsQuoted bool) int {
	for i < len(s) {
		c := s[i]
		if !startsQuoted && c == '\\' && i+1 < len(s) && (s[i+1] == '\\' || strings.IndexByte(escapedWordBytes, s[i+1]) >= 0) {
			i += 2
			continue
		}
		if strings.IndexByte(bareWordStopBytes, c) >= 0 {
			return i
		}
		if c == '$' && i+1 < len(s) && s[i+1] == '{' {
			if closeAt := wordBraceExpansionEnd(s, i+1); closeAt >= 0 {
				i = closeAt + 1
				continue
			}
			if startsQuoted {
				return i
			}
		}
		if startsQuoted && c == '$' && i+1 < len(s) && s[i+1] == '(' {
			return i
		}
		if startsQuoted && strings.IndexByte(codeAfterStringBytes, c) >= 0 {
			return i
		}
		i++
	}
	return i
}

// wordBraceExpansionEnd returns the index of the } that closes the ${ whose {
// is at s[open], or -1 when a blank, a line end, a nested { or the end of s
// comes first. Quoted strings inside it (${PW:-"x y"}) are skipped whole.
// Stopping at nesting and blanks keeps each read to one expansion's text.
func wordBraceExpansionEnd(s string, open int) int {
	for i := open + 1; i < len(s); i++ {
		switch c := s[i]; c {
		case '}':
			return i
		case '"', '\'':
			if i = quotedPieceEnd(s, i); i < 0 {
				return -1
			}
		case '{', ' ', '\t', '\n', '\r', '\f', '\v':
			return -1
		}
	}
	return -1
}

func captureRange(match []int, captureGroup int) (int, int, bool) {
	index := captureGroup * 2
	if captureGroup < 0 || len(match) <= index+1 || match[index] < 0 || match[index+1] < 0 {
		return 0, 0, false
	}
	return match[index], match[index+1], true
}

// unquoteRangeQuoted narrows [start,end) past a matching pair of surrounding
// quotes and reports whether those quotes were single quotes. Single-quoted
// shell values are literals — `'$uperSecret123'` is a real password, never a
// variable expansion — so callers must pass singleQuoted through to the
// placeholder check to keep the shell-variable heuristic from unredacting
// them.
func unquoteRangeQuoted(s string, start, end int) (int, int, bool) {
	if end-start < 2 {
		return start, end, false
	}
	first, last := s[start], s[end-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return start + 1, end - 1, first == '\''
	}
	return start, end, false
}

// hasNonPlaceholderPasswordAssignment reports whether candidate assigns a
// password that is not a placeholder. notPassword, when non-nil, excludes
// assignments that only look like one.
func hasNonPlaceholderPasswordAssignment(candidate string, notPassword func(key, value string) bool) bool {
	for _, loc := range passwordAssignmentRegex.FindAllStringSubmatchIndex(candidate, -1) {
		if len(loc) < 6 || loc[4] < 0 || loc[5] < 0 {
			continue
		}
		start, end, _ := unquoteRangeQuoted(candidate, loc[4], loc[5])
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

// hasNonPlaceholderQuotedPasswordValue is the quote-aware variant:
// singleQuoted marks values whose surrounding single quotes were stripped by
// unquoteRange, which makes any $-leading content a literal, not a shell
// expansion.
func hasNonPlaceholderQuotedPasswordValue(value string, singleQuoted bool) bool {
	return value != "" && !isPlaceholderSecretValueQuoted(value, singleQuoted)
}

// isPlaceholderSecretValue checks a bare scanner finding. It stays an exact
// match on known placeholder shapes: a finding is already a scanner's claim
// that the span is a secret, and the key/value rules below (ellipses, shell
// references, code literals) would drop real findings — the betterleaks
// generic rule's capture can end in `...` when prose follows the secret.
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

// codeLiteralPlaceholderValues are values a non-password credential key meets
// in code and tool output rather than in secrets: language null/boolean
// literals (`access_token = None`) and the exact masks Entire's own review
// trust inventory prints (`api_key=hidden`, `--api-key=(value hidden)`).
// Never applied to password keys — `PASSWORD=hidden` or `=true` can be the
// real password — nor to scanner findings.
var codeLiteralPlaceholderValues = map[string]struct{}{
	"none": {}, "null": {}, "nil": {}, "undefined": {},
	"true": {}, "false": {},
	"hidden": {}, "(value hidden)": {},
}

// isNonPasswordCodeLiteral reports whether value is a code literal or tool
// mask under a credential key that does not name a password.
func isNonPasswordCodeLiteral(key, value string) bool {
	if _, ok := codeLiteralPlaceholderValues[strings.ToLower(strings.TrimSpace(value))]; !ok {
		return false
	}
	return !isPasswordKey(key)
}

// isPasswordKey reports whether key names a password (DB_PASSWORD, passwd,
// admin_pwd), where a human-chosen value can look like a placeholder.
func isPasswordKey(key string) bool {
	for _, form := range credentialJSONKeyForms(key) {
		if credentialJSONKeyRegex.MatchString(form) || genericPasswordKeyRegex.MatchString(form) ||
			passwordWordInKeyRE.MatchString(form) {
			return true
		}
	}
	return false
}

// hasNonPlaceholderKeyValue reports whether value under key is a real
// credential. Password keys get only the shared placeholder rules (plus the
// single-quote literal rule): `your-secret` or `api-token-here` can be a
// human-chosen password. Other credential keys also accept the key/value
// placeholder shapes in isPlaceholderSecretValueQuoted.
func hasNonPlaceholderKeyValue(key, value string, singleQuoted bool) bool {
	if isPasswordKey(key) {
		if singleQuoted {
			return hasNonPlaceholderQuotedPasswordValue(value, true)
		}
		return hasNonPlaceholderPasswordValue(value)
	}
	return hasNonPlaceholderQuotedPasswordValue(value, singleQuoted)
}

// passwordWordInKeyRE spots a password segment anywhere in a normalized key.
var passwordWordInKeyRE = regexp.MustCompile(`(?:^|_)(?:pass|passwd|password|pwd|passphrase)(?:_|$)`)

func isPlaceholderSecretValueQuoted(value string, singleQuoted bool) bool {
	trimmedSpace := strings.TrimSpace(value)
	// Values that arrive still wearing their own single quotes are shell
	// literals too (some callers match quoted forms without unquoting).
	if len(trimmedSpace) >= 2 && trimmedSpace[0] == '\'' && trimmedSpace[len(trimmedSpace)-1] == '\'' {
		singleQuoted = true
	}
	trimmed := strings.Trim(trimmedSpace, `"'`)
	if trimmed == "" {
		return true
	}
	normalized := strings.ToLower(trimmed)
	// Inside single quotes the shell performs no expansion: `'$uperSecret123'`
	// and `'${looks_like_var}'` are literal passwords, so only the literal
	// placeholder shapes apply; the expansion and template rules do not.
	if singleQuoted {
		if isBracketedPlaceholder(trimmed) {
			return true
		}
		if _, ok := placeholderSecretValues[normalized]; ok {
			return true
		}
		return isRepeatedCharPlaceholder(normalized)
	}
	if isPlaceholderSecretValue(trimmed) || upperBracketedPlaceholderRE.MatchString(trimmed) {
		return true
	}
	// Documentation fill-ins: `your_api_key_here`, `your-token`, `paste_key_here`.
	if docFillInPlaceholderRE.MatchString(normalized) {
		return true
	}
	// Command substitutions (`TOKEN="$(cat token)"`) are how docs and scripts
	// reference a secret without containing it — the value is the recipe. Only
	// a known secret-fetching command with no quotes or redirection counts:
	// `$(echo x)`, `$(/bin/echo x)` and `$(cat <<< x)` carry the secret itself.
	if inner, ok := strings.CutPrefix(trimmed, "$("); ok && strings.HasSuffix(inner, ")") {
		if isSecretRecipeCommand(strings.TrimSuffix(inner, ")")) {
			return true
		}
	}
	// Documentation ellipsis placeholders: `ANTHROPIC_API_KEY=sk-ant-...`.
	// Only a bare prefix (empty, or ending at a `-`/`_` separator) counts:
	// `hunter2...` is a real value followed by prose, and a short one is
	// below what entropy catches.
	if stem, ok := strings.CutSuffix(normalized, "..."); ok && (stem == "" || ellipsisPrefixStemRE.MatchString(stem)) {
		return true
	}
	return goTemplateReferencePattern.MatchString(trimmed)
}

// ellipsisPrefixStemRE matches the vendor prefix before a documentation
// ellipsis (`sk-ant-`, `ghp_`, `sb_secret_`): short, letters only, ending in a
// separator. Digits or length mean key material, as in `hunter2-...`.
var ellipsisPrefixStemRE = regexp.MustCompile(`^[a-z]{1,10}(?:[-_][a-z]{1,10}){0,2}[-_]$`)

// docFillInPlaceholderRE matches documentation fill-in values that name a
// credential: "your_..._<noun>" optionally followed by "_here", or a
// credential noun followed by "_here" (`your_api_key_here`, `your-token`,
// `api-token-goes-here`). A lowercase phrase without a credential noun, such
// as `your-dog-name`, can be a real passphrase.
var docFillInPlaceholderRE = regexp.MustCompile(`^(?:your[_-](?:[a-z]+[_-])*(?:api[_-]?key|key|token|secret|password|passphrase|pat)(?:[_-](?:goes[_-])?here)?|[a-z_-]*(?:key|token|secret|password)[_-](?:goes[_-])?here)$`)

// secretRecipeCommands are commands that fetch a secret from somewhere else,
// so `$(cmd ...)` names where the value lives rather than containing it.
var secretRecipeCommands = map[string]struct{}{
	"cat": {}, "printenv": {}, "gh": {}, "op": {}, "vault": {}, "aws": {},
	"gcloud": {}, "az": {}, "security": {}, "pass": {}, "doppler": {},
	"bw": {}, "lpass": {}, "secret-tool": {}, "kubectl": {}, "infisical": {},
}

// isSecretRecipeCommand reports whether a $(...) body runs an allowlisted
// secret-fetching command. Quotes and redirection (`<<<`, `<`) can carry a
// literal, so any body containing them is treated as text.
func isSecretRecipeCommand(cmd string) bool {
	if strings.ContainsAny(cmd, "\"'<`$|;&") {
		return false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	_, ok := secretRecipeCommands[fields[0]]
	return ok
}

// shellParameterName matches the parameter a ${…} expansion names: a
// variable or array element (${DB_PASSWORD}, ${pw[0]}), a positional
// parameter (${1}, ${10}), or a special parameter (${@}, ${*}, ${#}, ${?},
// ${-}, ${$}, ${!}), each optionally behind ! for indirection (${!ref}).
const shellParameterName = `!?(?:[A-Za-z_][A-Za-z0-9_]*(?:\[[^\]]*\])?|[0-9]+|[@*#?$!-])`

// shellExpansionWordRE matches ${NAME<op>word}, whose word becomes the value
// when NAME is unset (-, =) or set (+); with a colon, an empty NAME counts as
// unset. ${NAME:?word} is left out: its word is an error message, never a
// value.
var shellExpansionWordRE = regexp.MustCompile(`^\$\{` + shellParameterName + `:?[-=+](.*)\}$`)

// shellLengthRE matches ${#NAME}, the length of a parameter, which holds no
// text of its own even when NAME is itself # (${##}).
var shellLengthRE = regexp.MustCompile(`^\$\{#` + shellParameterName + `\}$`)

// isPlaceholderShellExpansion reports whether a ${…} value is a placeholder:
// one complete expression that is a plain reference, or one whose word is
// itself empty or a placeholder, so ${DB_PASSWORD:-hunter2},
// ${1:-hunter2} and ${A}hunter2${B} still count as secrets. A single-quoted
// word is a literal, so ${PW:-'$S3CRET'} is a secret too.
func isPlaceholderShellExpansion(s string) bool {
	if !isSingleBraceExpression(s) {
		return false
	}
	if shellLengthRE.MatchString(s) {
		return true
	}
	if m := shellExpansionWordRE.FindStringSubmatch(s); m != nil {
		if word := m[1]; len(word) >= 2 && word[0] == '\'' && word[len(word)-1] == '\'' {
			return isPlaceholderSecretValueQuoted(word, true)
		}
		return isPlaceholderSecretValue(m[1])
	}
	return !shellTextOperatorRE.MatchString(s)
}

// doubleQuotedText returns text, which double quotes enclose, as the
// placeholder rules read it. Inside double quotes a single quote is a plain
// character: "${PW:-'$FALLBACK'}" expands $FALLBACK and puts apostrophes
// around it, so an expansion whose word is all single-quoted is read without
// those quotes, as ${PW:-$FALLBACK}. Without double quotes the same word is the
// literal text $FALLBACK, a secret (see isPlaceholderShellExpansion).
func doubleQuotedText(text string) string {
	m := shellExpansionWordRE.FindStringSubmatchIndex(text)
	if m == nil || !isSingleBraceExpression(text) {
		return text
	}
	if word := text[m[2]:m[3]]; len(word) >= 2 && word[0] == '\'' && word[len(word)-1] == '\'' {
		return text[:m[2]] + word[1:len(word)-1] + text[m[3]:]
	}
	return text
}

// shellTextOperatorRE matches ${NAME<op>...} for shell operators whose text can
// carry a literal value: pattern substitution and removal (/ # %), case
// changes (^ ,), and substrings (:offset). Non-shell forms such as ${var.x},
// ${env:X} and ${{secrets.X}} do not match.
var shellTextOperatorRE = regexp.MustCompile(`^\$\{` + shellParameterName + `(?:[/#%^,]|:[-0-9 ])`)

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
// shape: all-lowercase or all-uppercase letters joined by `-` or `_`
// (`<your-token>`, `<ENTIRE_TOKEN>`). Digits, mixed case, and special chars
// are rejected so values like `<hunter2>` or `<RealPassword>` still fall
// through to redaction.
var bracketedPlaceholderInteriorRE = regexp.MustCompile(`^[a-z][a-z_-]*$`)

// upperBracketedPlaceholderRE matches an all-uppercase "<NAME>" placeholder
// (`<ENTIRE_TOKEN>`). Key/value captures only: the shared rule stays
// lowercase, so `<HUNTER>` from a scanner or connection string still redacts.
var upperBracketedPlaceholderRE = regexp.MustCompile(`^<[A-Z][A-Z_-]{2,}>$`)

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
	for _, normalized := range credentialJSONKeyForms(key) {
		if isKnownNonSecretTokenKey(normalized) {
			continue
		}
		if isSensitiveNormalizedJSONValueKey(normalized) || isCredentialIdentifierKey(normalized) {
			return true
		}
		if credentialContext && genericPasswordKeyRegex.MatchString(normalized) {
			return true
		}
	}
	return false
}

// credentialJSONKeyForms returns the camel-split normalization of key and,
// when it differs, the flat one (lowercased, no camel split). Matching either
// keeps vendor-cased keys detected: camel splitting turns MySQLPassword into
// my_sql_password and hostName into host_name, which the key shapes and the
// connection-object case list only know in their flat forms.
func credentialJSONKeyForms(key string) []string {
	camel := normalizeCredentialJSONKey(key)
	flat := flattenCredentialJSONKey(key)
	if flat == camel {
		return []string{camel}
	}
	return []string{camel, flat}
}

// flattenCredentialJSONKey lowercases key and maps `-`, ` `, and `.` to `_`
// without splitting camelCase. Flattened-config exporters (Spring, dotnet,
// Vault) emit dotted keys like "db.password"; treat them like underscored.
func flattenCredentialJSONKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(key)
}

// isKnownNonSecretTokenKey reports whether the normalized JSON key is
// `<prefix>_token` where every `_`-separated segment of prefix is in
// nonSecretTokenPrefixes — so compound pagination keys like
// `next_page_token` qualify alongside `cancel_token` and
// `pagination_token`, while any non-allowlisted segment
// (`auth_page_token`, `csrf_token`) still redacts. Deliberately NOT
// applied to `_secret` keys: `previous_secret`/`next_secret` are real
// field names in webhook/HMAC secret-rotation payloads, and the entropy
// fallback cannot catch them when hex-encoded (16 symbols cap Shannon
// entropy at 4.0, below the 4.5 threshold).
func isKnownNonSecretTokenKey(normalized string) bool {
	prefix, ok := strings.CutSuffix(normalized, "_token")
	if !ok || prefix == "" {
		return false
	}
	for _, segment := range strings.Split(prefix, "_") {
		if _, allowed := nonSecretTokenPrefixes[segment]; !allowed {
			return false
		}
	}
	return true
}

func isNonSecretIdentifierAssignment(candidate string) bool {
	key, value, ok := strings.Cut(candidate, "=")
	if !ok || key == "" || value == "" {
		return false
	}
	for _, form := range credentialJSONKeyForms(key) {
		if isSensitiveNormalizedJSONValueKey(form) || genericPasswordKeyRegex.MatchString(form) {
			return false
		}
	}
	normalized := normalizeCredentialJSONKey(key)
	// A credential word anywhere in the key disqualifies suppression even
	// under an id/account suffix: aws_access_key_id names a credential, not
	// an identifier. This is the only reliable discriminator — Shannon
	// entropy is not monotonic under concatenation, so a combined key=value
	// can clear the 4.5 threshold while the value alone does not (e.g.
	// aws_access_key_id=AKIA… scores 4.50 combined vs 3.68 for the value),
	// and betterleaks' vendor rules filter the moderate-entropy forms too.
	// Deliberately broader than isCredentialIdentifierKey: this only decides
	// whether the entropy layer may stay silent, so failing closed costs
	// readability, not a leak.
	if credentialWordInKeyRE.MatchString(normalized) || bearerIdentifierKeyRE.MatchString(normalized) {
		return false
	}
	if isHighEntropySecretCandidate(value) {
		return false
	}
	return isIdentifierAssignmentKey(normalized)
}

// bearerIdentifierKeyRE spots identifier keys whose value can double as a
// bearer credential (session, request, nonce, CSRF, cookie ids). They keep
// the entropy layer's combined key=value scoring.
var bearerIdentifierKeyRE = regexp.MustCompile(`(?:^|_)(?:session|sess|sid|request|req|nonce|csrf|xsrf|cookie|jti)(?:_|$)`)

// isPureIdentifierKey reports whether key is an id/account key that names no
// credential or bearer-style value (CLIENT_ID, tenant_id).
func isPureIdentifierKey(key string) bool {
	normalized := normalizeCredentialJSONKey(key)
	return isIdentifierAssignmentKey(normalized) &&
		!credentialWordInKeyRE.MatchString(normalized) &&
		!bearerIdentifierKeyRE.MatchString(normalized)
}

func isIdentifierAssignmentKey(normalized string) bool {
	return normalized == "id" ||
		normalized == "account" ||
		strings.HasSuffix(normalized, "_id") ||
		strings.HasSuffix(normalized, "_account")
}

// isCredentialIdentifierKey reports whether an id/account-suffixed JSON key
// names a credential rather than an identifier that merely mentions one. Only
// the AWS access-key id (aws_access_key_id, AccessKeyId) qualifies: it is
// paired with a secret and treated as sensitive, while api_key_id, tokenId,
// SecretId and KeyId name a key, an AWS secret, or a KMS key — identifiers,
// not key material.
func isCredentialIdentifierKey(normalized string) bool {
	stem, ok := strings.CutSuffix(normalized, "_id")
	if !ok {
		stem, ok = strings.CutSuffix(normalized, "_account")
	}
	if !ok || stem == "" {
		return false
	}
	return accessKeyStemRE.MatchString(stem)
}

// accessKeyStemRE matches the AWS access-key id stem (access_key,
// aws_access_key, secret_access_key), which secretValueKeyShape leaves out
// because a bare access_key value is an identifier, not the secret.
var accessKeyStemRE = regexp.MustCompile(`^(?:[a-z0-9]+_)*access_key$`)

func isHighEntropySecretCandidate(value string) bool {
	for _, loc := range secretPattern.FindAllStringIndex(value, -1) {
		if shannonEntropy(value[loc[0]:loc[1]]) > entropyThreshold {
			return true
		}
	}
	return false
}

func isSensitiveNormalizedJSONValueKey(normalized string) bool {
	return credentialJSONKeyRegex.MatchString(normalized) ||
		secretJSONKeyRegex.MatchString(normalized)
}

func isCredentialJSONObject(obj map[string]any) bool {
	var hasHost, hasUser bool
	for key := range obj {
		for _, normalized := range credentialJSONKeyForms(key) {
			switch normalized {
			case "host", "hostname", "server", "addr", "address", "datasource", "data_source":
				hasHost = true
			case "user", "username", "userid", "user_id", "uid":
				hasUser = true
			}
		}
		if hasHost && hasUser {
			return true
		}
	}
	return false
}

func normalizeCredentialJSONKey(key string) string {
	key = strings.TrimSpace(key)
	needsNormalization := false
	hasNonASCII := false
	for i := range len(key) {
		current := key[i]
		if current >= utf8.RuneSelf {
			hasNonASCII = true
		}
		if hasNonASCII || current >= 'A' && current <= 'Z' || current == '-' || current == ' ' || current == '.' {
			needsNormalization = true
			break
		}
	}
	if !needsNormalization {
		return key
	}

	var normalized strings.Builder
	normalized.Grow(len(key))
	for i := range len(key) {
		current := key[i]
		switch current {
		case '-', ' ', '.':
			normalized.WriteByte('_')
			continue
		}
		if current >= 'A' && current <= 'Z' && i > 0 {
			previousIsLower := key[i-1] >= 'a' && key[i-1] <= 'z'
			nextIsLower := i+1 < len(key) && key[i+1] >= 'a' && key[i+1] <= 'z'
			previousIsUpper := key[i-1] >= 'A' && key[i-1] <= 'Z'
			if previousIsLower || (previousIsUpper && nextIsLower) {
				normalized.WriteByte('_')
			}
		}
		if current >= 'A' && current <= 'Z' {
			current += 'a' - 'A'
		}
		normalized.WriteByte(current)
	}
	result := normalized.String()
	if hasNonASCII {
		return strings.ToLower(result)
	}
	return result
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
	var replaced tally
	redacted, err := jsonlContentImpl(s, leafRedactor{detect: detectAllLayers, counts: &replaced}, concurrencySafeRedactor)
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
		return RedactedBytes{data: b, counts: replaced.counts()}, nil
	}
	return RedactedBytes{data: []byte(redacted), counts: replaced.counts()}, nil
}

// JSONLBytesWithPrivacyFilter augments JSONLBytes with the OpenAI Privacy
// Filter. Use only at condensation/export boundaries; per-turn writes must
// use JSONLBytes.
// Returns ErrScannerDegraded when the goredact scanner degraded while
// betterleaks is disabled.
//
// The result carries no counts (Counts returns nil): the OPF passes do not
// tally, so this output must not feed checkpoint metadata's redaction counts.
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
	return jsonlContentImpl(content, regexLayers, concurrencySafeRedactor)
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

// leafRedactor redacts the string leaves of one JSONL walk, and each line that
// is not JSON, with the regions detect finds. When counts is non-nil it also
// adds what it replaced there.
type leafRedactor struct {
	detect func(string) []taggedRegion
	counts *tally
}

// regexLayers is the uncounted regex-only redactor, String's walk equivalent.
var regexLayers = leafRedactor{detect: detectAllLayers}

func (r leafRedactor) redact(s string) string {
	return redactRegions(s, r.detect(s), r.counts)
}

// mark returns the counts so far, for rollback when a line is redacted again.
func (r leafRedactor) mark() tally {
	if r.counts == nil {
		return tally{}
	}
	return *r.counts
}

func (r leafRedactor) rollback(to tally) {
	if r.counts != nil {
		*r.counts = to
	}
}

// replacements returns the placeholders counted so far, 0 when not counting.
func (r leafRedactor) replacements() int {
	if r.counts == nil {
		return 0
	}
	return r.counts.replacements
}

// spliced replaces the placeholders counted since before with those the
// splice wrote. The splice rewrites every copy of a collected value in the
// line, including copies the walk never scanned, so it can write more than
// the scanned values account for. Layer matches stay as scanned.
func (r leafRedactor) spliced(before tally, written int) {
	if r.counts != nil {
		r.counts.replacements = before.replacements + written
	}
}

// jsonlContentImpl is the body of JSONLContent parameterized by a per-leaf
// redactor. JSONLContent passes the regex layers. The OPF-enabled flow uses
// two passes: one with a collector that records leaves and finds nothing, one
// that combines regex layers with cached OPF spans for the recorded leaves.
//
// concurrencySafe reports whether redactor.detect tolerates concurrent calls;
// when it does, large content is sharded across goroutines.
func jsonlContentImpl(content string, redactor leafRedactor, concurrencySafe bool) (string, error) {
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
func redactSingleJSONValue(content string, redactor leafRedactor) (result string, handled bool, err error) {
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
	before := redactor.mark()
	repls := collectJSONLReplacements(parsed, redactor)
	result, written, err := applyJSONReplacements(content, repls)
	if err != nil {
		return "", true, err
	}
	redactor.spliced(before, written)
	if len(repls) > 0 {
		result, err = ensureReplacementsLanded(content, result, repls, redactor, before)
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
//
// A var so TestMain can shrink it.
var jsonlShardTargetBytes = 1 << 20 // 1MiB

// redactJSONLLinesSharded splits lines into contiguous byte-balanced groups,
// redacts them concurrently, and rejoins them in order. Output is byte-identical
// to redactJSONLLines over the same lines, because each line is redacted in
// isolation and shard boundaries fall between lines.
//
// redactor.detect MUST be safe for concurrent use. Each shard counts into its
// own tally, summed in order once every shard is done.
func redactJSONLLinesSharded(lines []string, redactor leafRedactor) (string, error) {
	// Balance shards by bytes, not by line count: agent transcripts mix
	// thousands of short lines with occasional multi-MB tool results, so equal
	// line counts would leave one oversized shard as the tail.
	bounds := shardLineBounds(lines, jsonlShardTargetBytes)
	if len(bounds) < 2 {
		return redactJSONLLines(lines, redactor)
	}

	results := make([]string, len(bounds))
	shardCounts := make([]tally, len(bounds))
	g := new(errgroup.Group)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i, b := range bounds {
		g.Go(func() error {
			shard := leafRedactor{detect: redactor.detect}
			if redactor.counts != nil {
				shard.counts = &shardCounts[i]
			}
			out, err := redactJSONLLines(lines[b.lo:b.hi], shard)
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
	for _, c := range shardCounts {
		redactor.counts.add(c)
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
func redactJSONLLines(lines []string, redactor leafRedactor) (string, error) {
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
			b.WriteString(redactor.redact(line))
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
		before := redactor.mark()
		repls := collectJSONLReplacements(parsed, redactor)
		result, written, err := applyJSONReplacements(line, repls)
		if err != nil {
			return "", err
		}
		redactor.spliced(before, written)
		if len(repls) > 0 {
			result, err = ensureReplacementsLanded(line, result, repls, redactor, before)
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
// pass 2 applies the nine regex layers per leaf plus the cached OPF spans
// for that leaf. One OPF shell-out covers the whole transcript instead of
// one per leaf — without batching, a typical 500-leaf transcript would
// take many minutes per commit.
//
// Falls back to the plain JSONLContent flow when OPF is unconfigured, the
// breaker is tripped, no categories are enabled, or the batch call errors.
func JSONLContentWithPrivacyFilter(ctx context.Context, content string) (string, error) {
	cfg := getOPFConfig()
	if cfg == nil || !cfg.Enabled || cfg.runtime == nil || opfBreakerTripped.Load() {
		return jsonlContentImpl(content, regexLayers, concurrencySafeRedactor)
	}
	cats := enabledCategories(cfg)
	if len(cats) == 0 {
		return jsonlContentImpl(content, regexLayers, concurrencySafeRedactor)
	}

	// Pass 1: collect eligible (has-space, deduped) leaves. The collector
	// finds no regions, so the JSONL walker doesn't mutate content.
	seen := make(map[string]struct{})
	var inputs []string
	if _, err := jsonlContentImpl(content, leafRedactor{detect: func(v string) []taggedRegion {
		if strings.ContainsRune(v, ' ') {
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				inputs = append(inputs, v)
			}
		}
		return nil
	}}, concurrencyUnsafeRedactor); err != nil {
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
			return jsonlContentImpl(content, regexLayers, concurrencySafeRedactor)
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
			return jsonlContentImpl(content, regexLayers, concurrencySafeRedactor)
		}
		for i, in := range inputs {
			spansByInput[in] = batched[i]
		}
	}

	// Pass 3: per-leaf regex layers + cached OPF spans.
	return jsonlContentImpl(content, leafRedactor{detect: func(v string) []taggedRegion {
		return append(detectAllLayers(v), opfSpanRegions(v, spansByInput[v], cfg)...)
	}}, concurrencySafeRedactor)
}

// applyJSONReplacements applies collected (original, redacted) string pairs
// to the raw JSON text, replacing JSON-encoded originals with their redacted forms.
// Returns s unchanged if repls is empty. written is how many placeholders the
// replacements put in: each copy replaced carries its pair's placeholders.
func applyJSONReplacements(s string, repls []jsonReplacement) (result string, written int, err error) {
	if len(repls) == 0 {
		return s, 0, nil
	}
	// Keyed replacements go first. An unkeyed one replaces every copy of its
	// text, so applied earlier it would also rewrite a keyed copy that needs
	// a different result (a password under a credential key, an address under
	// a network key), and the outcome would follow the walk's map order.
	repls = slices.Clone(repls)
	slices.SortStableFunc(repls, func(a, b jsonReplacement) int {
		switch {
		case a.key != "" && b.key == "":
			return -1
		case a.key == "" && b.key != "":
			return 1
		}
		return 0
	})
	for _, r := range repls {
		origJSON, err := jsonEncodeString(r.original)
		if err != nil {
			return "", 0, err
		}
		replJSON, err := jsonEncodeString(r.redacted)
		if err != nil {
			return "", 0, err
		}
		if r.key == "" {
			written += strings.Count(s, origJSON) * r.placeholders
			s = strings.ReplaceAll(s, origJSON, replJSON)
			continue
		}
		keyJSON, err := jsonEncodeString(r.key)
		if err != nil {
			return "", 0, err
		}
		var replaced int
		s, replaced = replaceKeyedJSONValue(s, keyJSON, origJSON, replJSON)
		written += replaced * r.placeholders
	}
	return s, written, nil
}

// replaceKeyedJSONValue replaces every occurrence of origJSON that follows
// keyJSON + optional whitespace + ':' + optional whitespace, and reports how
// many it replaced. Restricts substitution to value positions so a key's own
// redacted text is not rewritten when it collides with another field's value.
func replaceKeyedJSONValue(s, keyJSON, origJSON, replJSON string) (string, int) {
	if !strings.Contains(s, keyJSON) {
		return s, 0
	}
	replaced := 0
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
			replaced++
			continue
		}
		i = keyEnd
	}
	return b.String(), replaced
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
// key, empty at the root. An array element takes the key of the array that
// holds it, so {"Authorization":["Bearer <token>"]}, as json.Marshal writes an
// http.Header, keeps the key context of {"Authorization":"Bearer <token>"}.
// spliceKey is the key a raw-text splice can scope a replacement to: the
// leaf's own key, but empty inside arrays and at the root, because
// replaceKeyedJSONValue only finds a value written directly after its key.
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
func walkRedactableStrings(v any, transform func(key, spliceKey string, credentialContext bool, val string) string) (any, bool) {
	var walk func(key, spliceKey string, credentialContext, skipStrings bool, v any) (any, bool)
	walk = func(key, spliceKey string, credentialContext, skipStrings bool, v any) (any, bool) {
		switch val := v.(type) {
		case map[string]any:
			if shouldSkipJSONLObject(val) {
				return v, false
			}
			childCredentialContext := credentialContext || isCredentialJSONObject(val)
			var out map[string]any
			for k, child := range val {
				newChild, childChanged := walk(k, k, childCredentialContext, shouldSkipJSONLField(k), child)
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
				newChild, childChanged := walk(key, "", credentialContext, skipStrings, child)
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
			redacted := transform(key, spliceKey, credentialContext, val)
			if redacted == val {
				return v, false
			}
			return redacted, true
		default:
			return v, false
		}
	}
	return walk("", "", false, false, v)
}

// redactLeafValue is the per-leaf redaction rule shared by the splice
// collection pass and the structural re-encode fallback: the placeholder for
// a value whose key reveals a secret the value alone does not, otherwise the
// redactor, then the credential-key placeholder for values it left alone or
// in which it found only PII, then IP addresses under a key that names one.
//
// The redactor always sees the unchanged value. Rewriting part of it first
// would stop a rule that needs the whole value, such as an anchored custom
// rule, from matching, and expose text it used to redact.
func redactLeafValue(key string, credentialContext bool, val string, redactor leafRedactor) string {
	if keyContextFindsMore(key, val) {
		redactor.counts.addPlaceholder(layerKeyContext)
		return RedactedPlaceholder
	}
	version := isVersionValue(key, val)
	regions := redactor.detect(val)
	if version {
		regions = slices.DeleteFunc(slices.Clone(regions), func(r taggedRegion) bool { return r.builtinIP })
	}
	// PII inside a credential value (an address or email next to the
	// password) does not make the rest of it safe to keep.
	if onlyPIIRegions(regions) && isCredentialJSONSecretKey(key, credentialContext) &&
		hasNonPlaceholderKeyValue(key, val, false) && !isNonPasswordCodeLiteral(key, val) {
		redactor.counts.addPlaceholder(layerCredentialKey)
		return RedactedPlaceholder
	}
	redacted := redactRegions(val, regions, redactor.counts)
	if version {
		return redacted
	}
	return redactIPsUnderNetworkKey(key, redacted, redactor.counts)
}

// onlyPIIRegions reports whether every non-empty region is a PII match, true
// when there are none.
func onlyPIIRegions(regions []taggedRegion) bool {
	for _, r := range regions {
		if r.end > r.start && r.layer != layerPII {
			return false
		}
	}
	return true
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
		// A network word in the key makes an IP look key-dependent, but
		// redactIPsUnderNetworkKey owns that case and keeps the IP token
		// instead of replacing the whole value.
		if r.builtinIP && isNetworkKey(key) {
			continue
		}
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
// passes the regex layers; the OPF-enabled flow combines them with cached
// batched OPF spans.
func collectJSONLReplacements(v any, redactor leafRedactor) []jsonReplacement {
	seen := make(map[replKey]int)
	var repls []jsonReplacement
	walkRedactableStrings(v, func(key, spliceKey string, credentialContext bool, val string) string {
		before := redactor.replacements()
		redacted := redactLeafValue(key, credentialContext, val, redactor)
		if redacted != val {
			k := replKey{key: spliceKey, original: val}
			repl := jsonReplacement{
				key: spliceKey, original: val, redacted: redacted,
				placeholders: redactor.replacements() - before,
			}
			if i, ok := seen[k]; !ok {
				seen[k] = len(repls)
				repls = append(repls, repl)
			} else if hidesMore(redacted, repls[i].redacted) {
				// The same text can redact differently under another key: an
				// address under a network key, a password under a credential
				// key. The splice writes one result into every copy, so keep
				// the one that hides more.
				repls[i] = repl
			}
		}
		// Collection only. The fast path splices the raw text, so the tree is
		// left unchanged and the copy-on-write walk allocates nothing.
		return val
	})
	return repls
}

// hidesMore reports whether redacted result a hides more than b: a
// whole-value placeholder hides everything, otherwise more placeholders hide
// more.
func hidesMore(a, b string) bool {
	if b == RedactedPlaceholder {
		return false
	}
	return a == RedactedPlaceholder || strings.Count(a, RedactedPlaceholder) > strings.Count(b, RedactedPlaceholder)
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
//
// The re-encode redacts every leaf again, so the counts roll back to before,
// taken ahead of the collection pass, and each value is counted once.
func ensureReplacementsLanded(content, spliced string, repls []jsonReplacement, redactor leafRedactor, before tally) (string, error) {
	if !spliceMissed(spliced, repls) {
		return spliced, nil
	}
	redactor.rollback(before)
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
	walkRedactableStrings(parsed, func(_, spliceKey string, _ bool, val string) string {
		if missed {
			return val
		}
		if _, ok := originalsByKey[""][val]; ok {
			missed = true
			return val
		}
		if _, ok := originalsByKey[spliceKey][val]; ok {
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
func reencodeRedacted(content string, redactor leafRedactor) (string, error) {
	trimmed := strings.TrimSpace(content)
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return "", fmt.Errorf("%w: re-parse content for structural redaction: %w", ErrRedactionIncomplete, err)
	}
	redactedTree, _ := walkRedactableStrings(parsed, func(key, _ string, credentialContext bool, val string) string {
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

	// Credential-shaped identifier keys carry secrets despite their suffix.
	// Keep them in the walker so structural key detection can redact even
	// moderate-entropy values that the value-only scanners would miss.
	if isCredentialIdentifierKey(normalizeCredentialJSONKey(key)) {
		return false
	}

	// Skip ID fields
	if strings.HasSuffix(lower, "id") || strings.HasSuffix(lower, "ids") {
		return true
	}

	// Account fields (google_adsense_account, aws_account, ...) are
	// deliberately NOT skipped here: shouldSkipJSONLField bypasses every
	// content layer and prunes whole subtrees, and "service_account" is the
	// canonical field name of the GCP credential container whose nested
	// private_key must stay scanned. Low-entropy account identifiers survive
	// value-level scanning on their own; the text-path false positive is
	// handled by isNonSecretIdentifierAssignment instead.

	// Skip common path and directory fields from agent transcripts.
	// These appear frequently in tool calls and are structural, not secrets.
	switch lower {
	case "filepath", "file_path", "cwd", "root", "directory", "dir", "path":
		return true
	}

	return false
}

// shouldSkipJSONLObject returns true if the object has "type":"image" or "type":"image_url".
func shouldSkipJSONLObject(obj map[string]any) bool {
	t, ok := obj["type"].(string)
	return ok && (strings.HasPrefix(t, "image") || t == "base64")
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
