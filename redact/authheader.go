package redact

import (
	"regexp"
	"strings"
)

// authQuote is any quote that may wrap a header name or value in code.
const authQuote = "[\"'`]"

// authorizationHeaderPattern matches the credential of an HTTP Authorization
// (or Proxy-Authorization) header written out in text or code: a raw request,
// a JSON, Python or Ruby map of headers, a JS object literal, a Go
// http.Header literal, or a call such as req.Header.Set("Authorization", ...).
//
// Without it, a bearer token reaches the checkpoint whenever its entropy is
// under entropyThreshold, which covers the most common opaque token shapes:
// UUIDs (~3.8 bits), hex tokens (at most 4.0), and base62 tokens shorter than
// about 30 characters. betterleaks only flags such a header in a curl -H
// argument, so the same token pasted into code or a raw request passes every
// other layer.
//
// Group 1 is the optional auth scheme and group 2 the credential, whose
// charset is RFC 7235 token68: a shell or template reference ($TOKEN,
// {{token}}, <token>) or a string concatenation ("Bearer " + token) never
// matches. The boundary before the header name mirrors bareSKKeyPattern, so a
// literal JSON escape (\n, \") may precede it in the raw-text fall-back path.
// A comma separates name and value only after a quoted name, as in
// Header.Set("Authorization", ...); in prose, "authorization, token ..." is a
// list. Only a quoted name's value may start on the next line, as in wrapped
// code: an empty raw header must not take the next header line as its value.
var authorizationHeaderPattern = regexp.MustCompile(
	`(?i)(?:^|[^A-Za-z0-9]|\\[bfnrt]|\\u[0-9A-Fa-f]{4})` +
		`(?:proxy-)?authorization` +
		// a quoted name (possibly JSON-escaped, possibly a subscript key)
		// with its separator, or a bare name with its separator
		`(?:\\?` + authQuote + `\]?[ \t]*(?:=>|[:=,])\s*|[ \t]*(?:=>|[:=])[ \t]*)` +
		// an http.Header value is a list: a Go []string literal, or a JSON
		// array as json.Marshal writes it
		`(?:(?:\[\]string\{|\[)\s*)?` +
		// optional opening quote of the value, with a Python string prefix
		`(?:[frbu]{0,2}\\?` + authQuote + `[ \t]*)?` +
		`(?:([a-z][a-z0-9-]*)[ \t]+)?` +
		`([A-Za-z0-9_.~+/-]{16,}=*)`)

// authSchemes are the schemes whose credential is redacted wherever it ends.
// Any other scheme, or a credential with no scheme at all (Linear's raw
// lin_api_ keys), is redacted only when it is the whole header value, so that
// prose such as "authorization: requires v2_scoped_tokens_only" stays visible.
var authSchemes = map[string]struct{}{
	"bearer":  {},
	"token":   {},
	"basic":   {},
	"bot":     {},
	"key":     {},
	"apikey":  {},
	"api-key": {},
	"ssws":    {},
}

// detectAuthorizationHeaders returns a region for the credential of every
// Authorization header value in s. A credential must contain a digit, which
// keeps documentation placeholders such as "Bearer YOUR_ACCESS_TOKEN" visible;
// real bearer tokens, UUIDs, hex tokens and base64 Basic credentials almost
// always have one, and those that don't are long and random enough for the
// entropy layer.
func detectAuthorizationHeaders(s string) []taggedRegion {
	if !containsFoldASCII(s, "authorization") {
		return nil
	}
	var regions []taggedRegion
	for _, loc := range authorizationHeaderPattern.FindAllStringSubmatchIndex(s, -1) {
		start, end := loc[4], loc[5]
		value := s[start:end]
		if !strings.ContainsAny(value, "0123456789") || isPlaceholderSecretValue(value) {
			continue
		}
		if !isKnownAuthScheme(s, loc[2], loc[3]) && !endsHeaderValue(s, end) {
			continue
		}
		regions = append(regions, taggedRegion{region: region{start, end}})
	}
	return regions
}

func isKnownAuthScheme(s string, start, end int) bool {
	if start < 0 {
		return false
	}
	_, ok := authSchemes[strings.ToLower(s[start:end])]
	return ok
}

// endsHeaderValue reports whether the value ending at end is the whole header
// value: trailing spaces and tabs, then the end of the line or text, a closing
// quote, or a JSON escape.
func endsHeaderValue(s string, end int) bool {
	for end < len(s) && (s[end] == ' ' || s[end] == '\t') {
		end++
	}
	return end == len(s) || strings.IndexByte("\r\n\"'`\\", s[end]) >= 0
}

// containsFoldASCII reports whether s contains the lowercase ASCII word
// substr in any letter case, without copying s.
func containsFoldASCII(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i]|0x20 == substr[0] && strings.EqualFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}
