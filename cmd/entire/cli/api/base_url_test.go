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

func TestAuthBaseURL_FallsBackToBaseURL(t *testing.T) {
	t.Setenv(BaseURLEnvVar, "https://example.test")
	t.Setenv(AuthBaseURLEnvVar, "")

	if got := AuthBaseURL(); got != "https://example.test" {
		t.Fatalf("AuthBaseURL() = %q, want fallback to BaseURL", got)
	}
}

func TestAuthBaseURL_OverrideTakesPrecedence(t *testing.T) {
	t.Setenv(BaseURLEnvVar, "https://data.example.test")
	t.Setenv(AuthBaseURLEnvVar, " https://auth.example.test/ ")

	if got := AuthBaseURL(); got != "https://auth.example.test" {
		t.Fatalf("AuthBaseURL() = %q, want trimmed/normalized override", got)
	}
}

func TestAuthBaseURL_CanonicalisesScheme_HostCase_DefaultPort(t *testing.T) {
	// Same canonicalisation tokenmanager.New applies internally — must match
	// or the keyring key login wrote diverges from the one the manager later
	// reads, producing spurious "not logged in" errors on every data-API call.
	t.Setenv(AuthBaseURLEnvVar, "HTTPS://AUTH.example.com:443/")

	if got := AuthBaseURL(); got != "https://auth.example.com" {
		t.Fatalf("AuthBaseURL() = %q, want canonicalised origin", got)
	}
}

func TestNormalizeOriginURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"https://example.com", "https://example.com"},
		{"https://example.com/", "https://example.com"},
		{"HTTPS://Example.COM", "https://example.com"},
		{"https://example.com:443", "https://example.com"},
		{"http://example.com:80", "http://example.com"},
		{"https://example.com:8443", "https://example.com:8443"},
		{"https://example.com/some/path?q=1#frag", "https://example.com"},
		{"  https://example.com/  ", "https://example.com"},
		{"not a url", "not a url"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeOriginURL(tc.in); got != tc.want {
			t.Errorf("NormalizeOriginURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
