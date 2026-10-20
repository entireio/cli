package redact

import (
	"regexp"
	"slices"
	"strings"
)

// Provider-specific deterministic secret patterns.
//
// Detection here is purely prefix + length based: it never depends on
// entropy or the surrounding key name, so it catches low-entropy
// credential formats the other secret layers don't reliably flag.
//
// The betterleaks layer's coverage of these differs per prefix (verified
// against the upstream betterleaks v1.8.0 rule source):
//   - sb_secret_: the supabase-project-api-key rule is a *composite* rule
//     (a required supabase-project-url component, the 1.8.0 spelling of
//     RequiredRules) that only fires when a matching "*.supabase.co" URL is
//     present in the same content, on top of an exact 31-char body and an
//     entropy<=4.0 filter. A secret captured on its own therefore passes
//     straight through regardless of entropy.
//   - sbp_: the supabase-management-token rule fires standalone (no
//     required components), but only matches an exact 40-character
//     lowercase body and is further filtered by entropy<=3.5 and a
//     two-digit minimum. A high-entropy 40-char sbp_ token captured alone
//     IS caught by betterleaks; what this layer adds for sbp_ is coverage
//     of bodies at other lengths, lower entropy, or without two digits.
//
// Supabase (https://supabase.com/docs/guides/getting-started/api-keys):
//   - sb_secret_...      secret API key (replaces the legacy service_role
//     key; bypasses row-level security, server-side
//     only) — SENSITIVE, always redacted.
//   - sbp_...            personal access token used by the Supabase CLI and
//     Management API — SENSITIVE, always redacted.
//   - sb_publishable_... publishable key (replaces the legacy anon key). It
//     is designed to be embedded in client-side code and
//     is protected by row-level security, so it is NOT a
//     secret and is intentionally NOT redacted here.
//     Redacting it would be false-positive noise; this
//     matches betterleaks, which also ships no
//     publishable-key rule.
//
// The current real key bodies are 31 chars (sb_secret_) and 40 chars
// (sbp_); charsets mirror betterleaks (sb_secret_ keys are mixed-case
// base64url, sbp_ tokens are lowercase). The {20,} floor comfortably
// catches the current and plausibly-longer future formats while rejecting
// short identifier-like collisions such as "sb_secret_short".
//
// Known false-positive class: because the body charset includes `_` and
// the {20,} length check is open-ended, sufficiently long snake_case
// identifiers that merely start with a provider prefix are redacted even
// though they aren't secrets — e.g. `sb_secret_key_rotation_handler`, or
// mid-word inside a longer identifier like
// `libsbp_something_long_enough_value`. This is
// accepted: over-redaction is the safe direction here (see
// TestString_SupabaseProviderTokenLongIdentifierOverRedaction), and adding
// anchors or capping the body length to eliminate it would reopen the
// low-entropy under-redaction gap below.
//
// The prefix is deliberately NOT preceded by a \b word boundary. \b requires
// the character before the prefix to be a non-word char, so a secret glued to
// a preceding word character — an underscore-joined name (FOO_sb_secret_…) or,
// in the JSONL fall-back / raw redact.Bytes path, a JSON escape whose trailing
// letter abuts the prefix (…line1\nsb_secret_…, where the byte before "sb" is
// the literal 'n') — would slip past. Because these low-entropy secrets are
// backed up by no other layer, missing them means the raw key reaches the
// checkpoint blob. Dropping the anchor is redaction-completeness-safe: any
// high-entropy incidental match would already be caught by the entropy layer,
// and a mid-word identifier collision (documented above) only ever
// over-redacts, never under-redacts.
var providerTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sb_secret_[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sbp_[a-z0-9_-]{20,}`),
}

// tokenBoundary is the leading boundary for the bounded token rules below: the
// token must follow a non-alphanumeric char ("_" included) or a literal JSON
// escape, whose trailing letter or hex digit would otherwise read as part of a
// word ("\nsk-…", ""AKIA…"). Each rule captures the token in group 1.
const tokenBoundary = `(?:^|[^A-Za-z0-9]|\\[bfnrt]|\\u[0-9A-Fa-f]{4})` //nolint:gosec // regex literal, not a credential

// boundedTokenRule is a token shape with a leading boundary and a check that
// filters a match by the surrounding input and the token itself.
//
// A pattern that opens with tokenBoundary gives Go's regexp no literal prefix
// to skip ahead to, so every rule costs a full pass over every input. literals
// lists substrings at least one of which every match contains; inputs with
// none of them skip the pass without changing what matches.
type boundedTokenRule struct {
	literals []string
	pattern  *regexp.Regexp
	keep     func(s string, start, end int) bool
}

var boundedTokenRules = []boundedTokenRule{
	// Short sk- keys (e.g. LiteLLM virtual keys, sk- + 22 chars) that are too
	// short for the entropy layer (#2728). The boundary keeps "risk-" out, and
	// a digit or uppercase letter in the body keeps kebab-case names out.
	{
		literals: []string{"sk-"},
		pattern:  regexp.MustCompile(tokenBoundary + `(sk-[A-Za-z0-9_-]{20,})`),
		keep: func(s string, start, end int) bool {
			return strings.ContainsFunc(s[start+len("sk-"):end], isDigitOrUpper)
		},
	},
	// AWS access key IDs on their own (#2808). betterleaks' aws-access-token
	// rule fired only beside a secret access key, and a 20-char ID can never
	// clear the entropy threshold. The body is betterleaks' own regex; the
	// filters are its documentation EXAMPLE keys and low-entropy placeholders.
	{
		literals: []string{"A3T", "AKIA", "ASIA", "ABIA", "ACCA"},
		pattern:  regexp.MustCompile(tokenBoundary + `((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16})`),
		keep: func(s string, start, end int) bool {
			id := s[start:end]
			return endsAtBoundary(s, end) && !strings.HasSuffix(id, "EXAMPLE") && shannonEntropy(id) > 3.0
		},
	},
	// Tableau personal access tokens ("<22 base64>==:<32 alnum>"). betterleaks
	// only scans for them when "tableau" appears in the same text, and "/",
	// "+" and ":" split them for the entropy layer.
	{
		literals: []string{"==:"},
		pattern:  regexp.MustCompile(tokenBoundary + `([A-Za-z0-9+/]{22}==:[A-Za-z0-9]{32})`),
		keep: func(s string, start, end int) bool {
			return endsAtBoundary(s, end) && shannonEntropy(s[start:end]) >= 3.5
		},
	},
}

// detectProviderTokens returns tagged regions for every occurrence of a
// known provider secret-token prefix in s. Regions use the empty label so
// they render as the bare "REDACTED" token, consistent with the other
// always-on secret layers.
func detectProviderTokens(s string) []taggedRegion {
	var regions []taggedRegion
	for _, pat := range providerTokenPatterns {
		for _, loc := range pat.FindAllStringIndex(s, -1) {
			regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
		}
	}
	regions = append(regions, detectSalesforceTokens(s)...)
	for _, rule := range boundedTokenRules {
		if !containsAny(s, rule.literals) {
			continue
		}
		for _, loc := range rule.pattern.FindAllStringSubmatchIndex(s, -1) {
			start, end := loc[2], loc[3]
			if rule.keep(s, start, end) {
				regions = append(regions, taggedRegion{region: region{start, end}})
			}
		}
	}
	return regions
}

// salesforceHeadPattern matches the head of a Salesforce access token: the
// 15-char org ID and the "!" before the session body.
var salesforceHeadPattern = regexp.MustCompile(tokenBoundary + `(00[A-Za-z0-9]{13}!)`)

// detectSalesforceTokens catches Salesforce access tokens (org ID, "!", session
// body of 80+ chars). betterleaks only scans for them when "salesforce.com"
// appears in the same text, and "!" and "." split them into chunks the entropy
// layer misses.
//
// The body charset includes "_", "." and "-", so a single greedy regex would
// run a body into the org ID of a token joined to it ("<token>_<token>") and,
// stopping at that token's "!", skip it. Heads cannot overlap, so they are
// matched first and each body is extended by hand up to the next head.
func detectSalesforceTokens(s string) []taggedRegion {
	if !strings.Contains(s, "!") {
		return nil
	}
	heads := salesforceHeadPattern.FindAllStringSubmatchIndex(s, -1)
	var regions []taggedRegion
	for i, head := range heads {
		start, bodyStart := head[2], head[3]
		limit := len(s)
		if i+1 < len(heads) {
			limit = heads[i+1][0]
		}
		end := bodyStart
		for end < limit && isSalesforceBodyChar(s[end]) {
			end++
		}
		if end-bodyStart >= 80 && shannonEntropy(s[start:end]) >= 3.3 {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}
	return regions
}

func isSalesforceBodyChar(b byte) bool {
	return isASCIIAlnum(b) || b == '.' || b == '_' || b == '-'
}

func containsAny(s string, literals []string) bool {
	return slices.ContainsFunc(literals, func(lit string) bool { return strings.Contains(s, lit) })
}

// endsAtBoundary reports whether the match ending at end is not followed by an
// alphanumeric char. RE2 has no lookahead, so fixed-length rules check it here.
func endsAtBoundary(s string, end int) bool {
	return end == len(s) || !isASCIIAlnum(s[end])
}

func isASCIIAlnum(b byte) bool {
	return ('0' <= b && b <= '9') || ('A' <= b && b <= 'Z') || ('a' <= b && b <= 'z')
}

func isDigitOrUpper(r rune) bool {
	return ('0' <= r && r <= '9') || ('A' <= r && r <= 'Z')
}
