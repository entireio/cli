package api

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ErrInsecureHTTP is returned when the base URL uses HTTP without an explicit opt-in.
var ErrInsecureHTTP = errors.New("refusing to use insecure http:// base URL for authentication (use --insecure-http-auth to override)")

const (
	// DefaultBaseURL is the production Entire API origin.
	DefaultBaseURL = "https://entire.io"

	// DefaultAuthBaseURL is the production Entire auth origin (device flow,
	// auth-token management, keyring key). The CLI is split-host by default:
	// auth on us.auth.entire.io, data on entire.io.
	DefaultAuthBaseURL = "https://us.auth.entire.io"

	// BaseURLEnvVar overrides the Entire API origin for local development.
	BaseURLEnvVar = "ENTIRE_API_BASE_URL"

	// AuthBaseURLEnvVar overrides only the auth/login origin (device flow,
	// auth-tokens management, keyring key). Falls back to DefaultAuthBaseURL
	// when unset; local-dev and single-host deployments must set this
	// alongside ENTIRE_API_BASE_URL.
	AuthBaseURLEnvVar = "ENTIRE_AUTH_BASE_URL"

	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// BaseURL returns the effective Entire API base URL.
// ENTIRE_API_BASE_URL takes precedence over the production default.
func BaseURL() string {
	if raw := strings.TrimSpace(os.Getenv(BaseURLEnvVar)); raw != "" {
		return normalizeBaseURL(raw)
	}

	return DefaultBaseURL
}

// AuthBaseURL returns the origin used for the device-flow login, auth-token
// management endpoints, and the keyring key under which the bearer token is
// stored. ENTIRE_AUTH_BASE_URL takes precedence; otherwise it falls back to
// DefaultAuthBaseURL (split-host by default).
//
// The result is canonicalised — lowercased scheme/host, default port stripped,
// path/query/fragment dropped, trailing slash collapsed — so the value that
// flows into store.SaveToken keys matches what tokenmanager.New emits after
// its own NormalizeOriginURL pass. Without this, a user setting
// ENTIRE_AUTH_BASE_URL=https://AUTH.example.com:443/ would log in successfully
// (saved under the raw form) but every subsequent data-API command would
// resolve "not logged in" because the manager probes under the normalised
// "https://auth.example.com".
func AuthBaseURL() string {
	raw := strings.TrimSpace(os.Getenv(AuthBaseURLEnvVar))
	if raw == "" {
		raw = DefaultAuthBaseURL
	}
	return NormalizeOriginURL(raw)
}

// IsSplitHost reports whether the CLI is configured for split-host —
// i.e. ENTIRE_AUTH_BASE_URL points at a different origin than the data
// API. Both sides are canonicalised via NormalizeOriginURL before
// comparison: AuthBaseURL already does this internally, but BaseURL
// only trims whitespace and a trailing slash, so a cosmetically-
// different ENTIRE_API_BASE_URL (uppercase host, explicit :443, path
// suffix) would otherwise look split when it isn't.
func IsSplitHost() bool {
	return AuthBaseURL() != NormalizeOriginURL(BaseURL())
}

// ResolveURL joins an API-relative path against the effective base URL.
func ResolveURL(path string) (string, error) {
	return ResolveURLFromBase(BaseURL(), path)
}

// ResolveURLFromBase joins an API-relative path against an explicit base URL.
// Only http and https schemes are accepted.
func ResolveURLFromBase(baseURL, path string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}

	if base.Scheme != schemeHTTP && base.Scheme != schemeHTTPS {
		return "", fmt.Errorf("unsupported base URL scheme %q (must be http or https)", base.Scheme)
	}

	rel, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse path: %w", err)
	}

	return base.ResolveReference(rel).String(), nil
}

// RequireSecureURL returns ErrInsecureHTTP if the base URL uses the http scheme.
// Call this before making authenticated requests unless --insecure-http-auth is set.
func RequireSecureURL(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base URL: %w", err)
	}

	if u.Scheme == schemeHTTP {
		return ErrInsecureHTTP
	}

	return nil
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// NormalizeOriginURL canonicalises an origin URL the same way auth-go's
// tokenmanager does internally: lowercase scheme/host, default port stripped
// (80 for http, 443 for https), path/query/fragment dropped, trailing slash
// collapsed. On parse failure, raw is returned unchanged so non-URL audience
// values still compare byte-for-byte.
//
// Mirrors auth-go's internal/oauthhttp.NormalizeOriginURL so the value the
// CLI hands to the manager as Issuer survives the manager's own normalisation
// pass byte-for-byte — see AuthBaseURL.
func NormalizeOriginURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimmed
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	dropPort := port == "" ||
		(scheme == schemeHTTP && port == "80") ||
		(scheme == schemeHTTPS && port == "443")

	out := url.URL{Scheme: scheme}
	switch {
	case dropPort && strings.Contains(hostname, ":"):
		out.Host = "[" + hostname + "]"
	case dropPort:
		out.Host = hostname
	case strings.Contains(hostname, ":"):
		out.Host = "[" + hostname + "]:" + port
	default:
		out.Host = hostname + ":" + port
	}
	return out.String()
}

// OriginOnly is a backwards-compatible alias for NormalizeOriginURL.
// Callers reading raw URLs (e.g. ENTIRE_SEARCH_URL) and feeding them into
// tokenmanager.TokenRequest.Resource use this to strip path/query/fragment
// before the lib's stricter origin-only validator runs.
func OriginOnly(raw string) string {
	return NormalizeOriginURL(raw)
}
