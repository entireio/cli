package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/betterleaks/betterleaks/detect"
)

// secretPattern matches high-entropy strings that may be secrets.
// Note: / is excluded to prevent matching entire file paths as single tokens.
// Base64 and JWT tokens are still caught via high-entropy segments between slashes.
var secretPattern = regexp.MustCompile(`[A-Za-z0-9+_=-]{10,}`)

// credentialedURIPattern matches URLs that embed userinfo with a password, such
// as postgres://user:pass@host/db or redis://:pass@host/0. These often have
// moderate entropy and are not reliably covered by vendor-specific scanners.
var credentialedURIPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{1,31}://[^\s/?#@"'` + "`" + `<>:]*:[^\s/?#@"'` + "`" + `<>]+@[^\s"'` + "`" + `<>]+`)

// dbPasswordKeyShape matches a DB-prefixed credential key (vendor prefix +
// optional `_word`/`-word` segments + `password`/`passwd`/`pwd`). Used to
// compose both the env-var assignment regex and the JSON-key regex so the
// vendor list stays in one place.
const dbPasswordKeyShape = `(?:db|database|pg|postgres|postgresql|mysql|mariadb|redis|mongo|mongodb|sqlserver|mssql|jdbc)(?:[_-]+[a-z0-9]+)*[_-]*(?:password|passwd|pwd)` //nolint:gosec // regex literal, not a credential

var (
	jdbcPattern          = regexp.MustCompile(`(?i)\bjdbc:[^\s"'<>` + "`" + `]+`)
	databaseURLPattern   = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|redis)://[^\s"'<>` + "`" + `]+`)
	keywordDSNPattern    = regexp.MustCompile(`(?i)\b[a-z_][a-z0-9_]*=(?:"[^"]*"|'[^']*'|[^\s"']+)(?:\s+[a-z_][a-z0-9_]*=(?:"[^"]*"|'[^']*'|[^\s"']+)){2,}`)
	semicolonConnPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9 _-]*=(?:\{[^}]*\}|"[^"]*"|'[^']*'|[^=;"'\s]+)(?:;[a-z][a-z0-9 _-]*=(?:\{[^}]*\}|"[^"]*"|'[^']*'|[^=;"'\s]+)){2,}`)
	// credentialValuePattern requires the prefix to start at a non-alphanumeric
	// boundary, so APP_DB_PASSWORD matches via the leading `_` but mydbpassword
	// does not.
	credentialValuePattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(` + dbPasswordKeyShape + `)\s*=\s*("[^"]*"|'[^']*'|[^\s,;&]+)`)

	keywordHostPattern      = regexp.MustCompile(`(?i)(?:^|\s)host=`)
	keywordUserPattern      = regexp.MustCompile(`(?i)(?:^|\s)user=`)
	semicolonServerPattern  = regexp.MustCompile(`(?i)(?:^|;)\s*(?:server|data source|datasource|addr|address|network address)\s*=`)
	semicolonUserPattern    = regexp.MustCompile(`(?i)(?:^|;)\s*(?:user id|userid|user|uid)\s*=`)
	passwordAssignmentRegex = regexp.MustCompile(`(?i)(?:^|[?&;\s])(?:password|pwd)=("[^"]*"|'[^']*'|[^&;\s"']+)`)
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

// RedactedPlaceholder is the replacement text used for redacted secrets.
const RedactedPlaceholder = "REDACTED"

// placeholderSecretValues lists lowercase values that should be treated as
// non-secrets when they appear as a credential value: prior redactions
// (REDACTED / [REDACTED] / <REDACTED>), common documentation placeholders,
// and obviously-non-real defaults. Values matched by shape (mask runs,
// `<…>` brackets, `${…}` shell expansion) are handled separately.
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
// 1. Entropy-based: high-entropy alphanumeric sequences (threshold 4.5)
// 2. Pattern-based: betterleaks regex rules (260+ known secret formats)
// 3. Credentialed URIs: URLs containing userinfo passwords
// 4. Database connection strings: JDBC, keyword DSNs, and semicolon strings
// 5. Bounded credential key/value pairs: DB_PASSWORD=...
// 6. PII detection: email, phone, address patterns (only when configured via ConfigurePII)
// A string is redacted if ANY method flags it.
func String(s string) string {
	var regions []taggedRegion

	// 1. Entropy-based detection (secrets — always on).
	for _, loc := range secretPattern.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]

		// Don't consume characters that are part of JSON/string escape sequences.
		// Example: in "controller.go\nmodel.go", the regex could match "nmodel"
		// (consuming the 'n' from '\n'), and after replacement the '\' would be
		// followed by 'R' from "REDACTED", creating invalid escape '\R'.
		// Only skip for known JSON escape letters to avoid trimming real secrets
		// that happen to follow a literal backslash in decoded content.
		if start > 0 && s[start-1] == '\\' {
			switch s[start] {
			case 'n', 't', 'r', 'b', 'f', 'u', '"', '\\', '/':
				start++
				if end-start < 10 {
					continue
				}
			}
		}

		if shannonEntropy(s[start:end]) > entropyThreshold {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}

	// 2. Pattern-based detection via betterleaks (secrets — always on).
	if d := getDetector(); d != nil {
		for _, f := range d.DetectString(s) {
			if f.Secret == "" {
				continue
			}
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

	// 3. Credentialed URIs (secrets — always on).
	for _, loc := range credentialedURIPattern.FindAllStringIndex(s, -1) {
		regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
	}

	// 4. Database and connection-string detection (secrets — always on).
	regions = append(regions, detectConnectionStrings(s)...)

	// 5. Bounded credential key/value detection (secrets — always on).
	regions = append(regions, detectCredentialValues(s)...)

	// 6. PII detection (opt-in — only runs when configured).
	regions = append(regions, detectPII(getPIIConfig(), s)...)

	if len(regions) == 0 {
		return s
	}

	// Merge overlapping regions and build result.
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

func detectConnectionStrings(s string) []taggedRegion {
	if !strings.ContainsRune(s, '=') {
		return nil
	}
	var regions []taggedRegion
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
		if rule.hasSecret(s[start:end]) {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	return regions
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
	return hasNonPlaceholderPasswordAssignment(candidate)
}

func hasDatabaseURLSecret(candidate string) bool {
	u, err := url.Parse(candidate)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	for key, values := range u.Query() {
		if !isPasswordQueryKey(key) {
			continue
		}
		for _, value := range values {
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
	return keywordHostPattern.MatchString(candidate) &&
		keywordUserPattern.MatchString(candidate) &&
		hasNonPlaceholderPasswordAssignment(candidate)
}

func hasSemicolonConnectionPassword(candidate string) bool {
	return semicolonServerPattern.MatchString(candidate) &&
		semicolonUserPattern.MatchString(candidate) &&
		hasNonPlaceholderPasswordAssignment(candidate)
}

func detectCredentialValues(s string) []taggedRegion {
	var regions []taggedRegion
	for _, loc := range credentialValuePattern.FindAllStringSubmatchIndex(s, -1) {
		if len(loc) < 6 || loc[4] < 0 || loc[5] < 0 {
			continue
		}
		start, end := unquoteRange(s, loc[4], loc[5])
		if hasNonPlaceholderPasswordValue(s[start:end]) {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	return regions
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

func hasNonPlaceholderPasswordAssignment(candidate string) bool {
	for _, loc := range passwordAssignmentRegex.FindAllStringSubmatchIndex(candidate, -1) {
		if len(loc) >= 4 && loc[2] >= 0 && loc[3] >= 0 {
			start, end := unquoteRange(candidate, loc[2], loc[3])
			if hasNonPlaceholderPasswordValue(candidate[start:end]) {
				return true
			}
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
	normalized := strings.ToLower(trimmed)
	if strings.HasPrefix(normalized, "${") && strings.HasSuffix(normalized, "}") {
		return true
	}
	if _, ok := placeholderSecretValues[normalized]; ok {
		return true
	}
	return isRepeatedCharPlaceholder(normalized)
}

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
func JSONLBytes(b []byte) (RedactedBytes, error) {
	s := string(b)
	redacted, err := JSONLContent(s)
	if err != nil {
		return RedactedBytes{}, err
	}
	if redacted == s {
		return RedactedBytes{data: b}, nil
	}
	return RedactedBytes{data: []byte(redacted)}, nil
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
func JSONLContent(content string) (string, error) {
	// Try parsing the entire content as a single JSON value first.
	// Uses a streaming decoder to avoid copying the full content into []byte.
	// After decoding, attempts a second Decode to confirm EOF — if it succeeds,
	// the content is JSONL (multiple values) and we fall through to line-by-line.
	trimmed := strings.TrimSpace(content)
	if len(trimmed) > 0 {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		var parsed any
		if err := dec.Decode(&parsed); err == nil && isSingleJSONValue(dec) {
			// Content is a single JSON value (object/array) — redact field-aware.
			result, err := applyJSONReplacements(content, collectJSONLReplacements(parsed))
			if err != nil {
				return "", err
			}
			return result, nil
		}
	}

	// Fall back to line-by-line JSONL processing.
	lines := strings.Split(content, "\n")
	var b strings.Builder
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
			b.WriteString(String(line))
			continue
		}
		result, err := applyJSONReplacements(line, collectJSONLReplacements(parsed))
		if err != nil {
			return "", err
		}
		b.WriteString(result)
	}
	return b.String(), nil
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

// collectJSONLReplacements walks a parsed JSON value and collects unique string
// replacements for values that need redaction.
func collectJSONLReplacements(v any) []jsonReplacement {
	seen := make(map[string]bool)
	var repls []jsonReplacement
	var walk func(key string, credentialContext bool, v any)
	walk = func(key string, credentialContext bool, v any) {
		switch val := v.(type) {
		case map[string]any:
			if shouldSkipJSONLObject(val) {
				return
			}
			childCredentialContext := credentialContext || isCredentialJSONObject(val)
			for k, child := range val {
				if shouldSkipJSONLField(k) {
					continue
				}
				walk(k, childCredentialContext, child)
			}
		case []any:
			for _, child := range val {
				walk("", credentialContext, child)
			}
		case string:
			redacted := String(val)
			if redacted == val && isCredentialJSONSecretKey(key, credentialContext) && hasNonPlaceholderPasswordValue(val) {
				redacted = RedactedPlaceholder
			}
			if redacted != val {
				seenKey := key + "\x00" + val
				if !seen[seenKey] {
					seen[seenKey] = true
					repls = append(repls, jsonReplacement{key: key, original: val, redacted: redacted})
				}
			}
		}
	}
	walk("", false, v)
	return repls
}

// shouldSkipJSONLField returns true if a JSON key should be excluded from scanning/redaction.
// Skips "signature" (exact), ID fields (ending in "id"/"ids"), and common path/directory fields.
func shouldSkipJSONLField(key string) bool {
	if key == "signature" {
		return true
	}
	lower := strings.ToLower(key)

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
