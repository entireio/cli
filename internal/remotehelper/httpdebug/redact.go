// Package httpdebug logs HTTP request / response traffic for
// developer-facing debugging while redacting credentials.
//
// Three redaction layers run independently:
//
//   - sensitive headers (Authorization, Proxy-Authorization) collapse to
//     a placeholder before any header dump is written;
//   - URL userinfo (user@ or user:pass@) is stripped from any text run
//     through RedactURLCredentials, covering Location headers and HTML
//     redirect bodies;
//   - JSON token fields (access_token, id_token, refresh_token,
//     subject_token) — the shapes the STS / OAuth endpoints return —
//     collapse to the placeholder.
//
// The redactors run in that order in BodyPreview. Truncation comes last:
// if a long JWT or URL password extends past the preview boundary,
// truncating first would leave the regex's terminator outside the slice
// and silently leak the secret. Tests in this package pin the order.
package httpdebug

import (
	"bytes"
	"net/http"
	"regexp"
	"slices"
)

// Placeholder replaces any redacted value in dumps.
const Placeholder = "***REDACTED***"

// SensitiveHeaders carries credentials we never want to log verbatim.
// Stored by canonical key so RedactHeaders can do a single map lookup
// per header.
var SensitiveHeaders = map[string]struct{}{
	"Authorization":       {},
	"Proxy-Authorization": {},
}

// RedactHeaders returns a copy of h with values for SensitiveHeaders
// replaced by Placeholder. The input is not mutated — callers still
// need the real headers on the live request/response.
func RedactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vals := range h {
		ck := http.CanonicalHeaderKey(k)
		if _, sensitive := SensitiveHeaders[ck]; sensitive {
			redacted := make([]string, len(vals))
			for i := range vals {
				redacted[i] = Placeholder
			}
			out[ck] = redacted
			continue
		}
		out[ck] = slices.Clone(vals)
	}
	return out
}

// jsonTokenRedactor matches JSON token fields the STS/OAuth endpoints
// return (access_token, id_token, refresh_token, subject_token) and
// captures the surrounding key + quote so the replacement keeps the
// JSON shape intact.
var jsonTokenRedactor = regexp.MustCompile(`"(access_token|id_token|refresh_token|subject_token)"\s*:\s*"[^"]*"`)

// urlUserinfoRedactor matches the userinfo of an http/https URL —
// everything between `://` and the next `@` that doesn't cross a
// path/query/fragment boundary or whitespace. Covers `user@`,
// `user:pass@`, and `x-token:<pw>@` shapes that appear in Location
// headers and HTML redirect bodies.
var urlUserinfoRedactor = regexp.MustCompile(`(https?://)[^@/?#\s"'<>]+@`)

// RedactURLCredentials replaces the userinfo of any http/https URL in
// s with Placeholder. Safe to run over arbitrary text — header dumps,
// HTML bodies, log lines.
func RedactURLCredentials(s []byte) []byte {
	return urlUserinfoRedactor.ReplaceAll(s, []byte(`${1}`+Placeholder+`@`))
}

// RedactJSONTokens replaces JWT values inside a JSON body with
// Placeholder. Bodies that aren't JSON (or don't carry token fields)
// pass through unchanged.
func RedactJSONTokens(body []byte) []byte {
	return jsonTokenRedactor.ReplaceAll(body, []byte(`"$1":"`+Placeholder+`"`))
}

// PreviewBytes is the upper bound on the size of a body preview written
// to the log. Anything beyond is truncated.
const PreviewBytes = 512

// packMagic marks the start of a packfile inside a smart-HTTP response.
// Everything past it is binary pack data, unreadable in debug logs.
var packMagic = []byte("PACK")

// BodyPreview returns the first PreviewBytes of body after URL +
// JSON-token redaction. Redaction MUST happen before truncation: a long
// JWT or URL password can extend past PreviewBytes, leaving its
// terminator (closing quote or `@`) outside the preview slice and out of
// reach of the regexes — truncate-first silently leaks the secret.
//
// When the body contains a packfile, the preview ends just after the
// PACK signature so the rest of the binary stream doesn't flood the log.
func BodyPreview(body []byte) []byte {
	redacted := RedactJSONTokens(body)
	redacted = RedactURLCredentials(redacted)
	if idx := bytes.Index(redacted, packMagic); idx >= 0 {
		head := redacted[:min(idx+len(packMagic), PreviewBytes)]
		return append(append([]byte(nil), head...), []byte(" [PACK_DATA]")...)
	}
	if len(redacted) > PreviewBytes {
		return redacted[:PreviewBytes]
	}
	return redacted
}
