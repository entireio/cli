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

	// BaseURLEnvVar overrides the Entire API origin for local development.
	BaseURLEnvVar = "ENTIRE_API_BASE_URL"
)

// BaseURL returns the effective Entire API base URL.
// ENTIRE_API_BASE_URL takes precedence over the production default.
func BaseURL() string {
	if raw := strings.TrimSpace(os.Getenv(BaseURLEnvVar)); raw != "" {
		return normalizeBaseURL(raw)
	}

	return DefaultBaseURL
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

	if base.Scheme != "http" && base.Scheme != "https" {
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

	if u.Scheme == "http" {
		return ErrInsecureHTTP
	}

	return nil
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
