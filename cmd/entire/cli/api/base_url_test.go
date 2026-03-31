package api

import (
	"errors"
	"testing"
)

func TestBaseURL_Default(t *testing.T) {
	t.Setenv(BaseURLEnvVar, "")

	if got := BaseURL(); got != DefaultBaseURL {
		t.Fatalf("BaseURL() = %q, want %q", got, DefaultBaseURL)
	}
}

func TestBaseURL_Override(t *testing.T) {
	t.Setenv(BaseURLEnvVar, " http://localhost:8787/ ")

	if got := BaseURL(); got != "http://localhost:8787" {
		t.Fatalf("BaseURL() = %q, want %q", got, "http://localhost:8787")
	}
}

func TestResolveURLFromBase_RejectsNonHTTPScheme(t *testing.T) {
	t.Parallel()

	for _, scheme := range []string{"ftp://example.com", "file:///etc/passwd", "ssh://host"} {
		_, err := ResolveURLFromBase(scheme, "/path")
		if err == nil {
			t.Errorf("ResolveURLFromBase(%q, ...) = nil error, want scheme error", scheme)
		}
	}
}

func TestRequireSecureURL_AllowsHTTPS(t *testing.T) {
	t.Parallel()

	if err := RequireSecureURL("https://entire.io"); err != nil {
		t.Fatalf("RequireSecureURL(https) = %v, want nil", err)
	}
}

func TestRequireSecureURL_RejectsHTTP(t *testing.T) {
	t.Parallel()

	err := RequireSecureURL("http://localhost:8787")
	if err == nil {
		t.Fatal("RequireSecureURL(http) = nil, want error")
	}

	if !errors.Is(err, ErrInsecureHTTP) {
		t.Fatalf("RequireSecureURL(http) = %v, want ErrInsecureHTTP", err)
	}
}

func TestResolveURL(t *testing.T) {
	t.Setenv(BaseURLEnvVar, "http://localhost:8787/")

	got, err := ResolveURL("/oauth/device/code")
	if err != nil {
		t.Fatalf("ResolveURL() error = %v", err)
	}

	if got != "http://localhost:8787/oauth/device/code" {
		t.Fatalf("ResolveURL() = %q, want %q", got, "http://localhost:8787/oauth/device/code")
	}
}
