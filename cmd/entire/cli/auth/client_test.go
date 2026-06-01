package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/entireio/auth-go/deviceflow"
	"github.com/entireio/auth-go/tokens"
)

func TestOAuthErrorParts_RecognisedSentinel(t *testing.T) {
	t.Parallel()

	code, _, ok := oauthErrorParts(deviceflow.ErrAuthorizationPending)
	if !ok {
		t.Fatal("oauthErrorParts(ErrAuthorizationPending) ok = false, want true")
	}
	if code != "authorization_pending" {
		t.Fatalf("code = %q, want %q", code, "authorization_pending")
	}
}

func TestOAuthErrorParts_UnknownOAuthCodeFromGenericWrapper(t *testing.T) {
	t.Parallel()

	// deviceflow wraps unrecognised codes as "oauth error: <code>".
	err := errors.New("oauth error: invalid_client")
	code, desc, ok := oauthErrorParts(err)
	if !ok {
		t.Fatalf("oauthErrorParts(%q) ok = false, want true", err)
	}
	if code != "invalid_client" {
		t.Fatalf("code = %q, want %q", code, "invalid_client")
	}
	if desc != "" {
		t.Fatalf("desc = %q, want empty", desc)
	}
}

func TestOAuthErrorParts_UnknownOAuthCodeWithDescription(t *testing.T) {
	t.Parallel()

	err := errors.New("oauth error: invalid_client: bad credentials")
	code, desc, ok := oauthErrorParts(err)
	if !ok {
		t.Fatalf("oauthErrorParts(%q) ok = false, want true", err)
	}
	if code != "invalid_client" {
		t.Fatalf("code = %q, want %q", code, "invalid_client")
	}
	if desc != "bad credentials" {
		t.Fatalf("desc = %q, want %q", desc, "bad credentials")
	}
}

func TestOAuthErrorParts_NonOAuthErrorRoutedAsTransient(t *testing.T) {
	t.Parallel()

	_, _, ok := oauthErrorParts(errors.New("dial tcp: connection refused"))
	if ok {
		t.Fatal("oauthErrorParts(network error) ok = true, want false (transient)")
	}
}

func TestSecondsUntil_ZeroExpiry(t *testing.T) {
	t.Parallel()

	if got := secondsUntil(&tokens.TokenSet{}); got != 0 {
		t.Fatalf("secondsUntil(no expiry) = %d, want 0", got)
	}
}

func TestSecondsUntil_FutureExpiry(t *testing.T) {
	// No t.Parallel: this test mutates the package-level nowFunc and would
	// race other parallel tests in this package that also read it.
	prev := nowFunc
	t.Cleanup(func() { nowFunc = prev })
	base := time.Unix(1_700_000_000, 0)
	nowFunc = func() time.Time { return base }

	ts := &tokens.TokenSet{ExpiresAt: base.Add(120 * time.Second)}
	if got := secondsUntil(ts); got != 120 {
		t.Fatalf("secondsUntil(+120s) = %d, want 120", got)
	}
}

func TestSecondsUntil_PastExpiryClampsToZero(t *testing.T) {
	// No t.Parallel: same reason as TestSecondsUntil_FutureExpiry.
	prev := nowFunc
	t.Cleanup(func() { nowFunc = prev })
	base := time.Unix(1_700_000_000, 0)
	nowFunc = func() time.Time { return base }

	ts := &tokens.TokenSet{ExpiresAt: base.Add(-30 * time.Second)}
	if got := secondsUntil(ts); got != 0 {
		t.Fatalf("secondsUntil(past expiry) = %d, want 0 (clamped)", got)
	}
}

func TestNewClient_AllowInsecureHTTPPermitsNonLoopback(t *testing.T) {
	// --insecure-http-auth must reach the deviceflow client; without this,
	// http://devbox.internal style auth hosts fail with ErrInsecureBaseURL
	// even when the operator has explicitly opted in.
	t.Setenv("ENTIRE_AUTH_BASE_URL", "http://devbox.internal:8787")
	c := NewClient(nil, true)
	if !c.inner.AllowInsecureHTTP {
		t.Fatal("NewClient(nil, true) AllowInsecureHTTP = false, want true")
	}
}

func TestNewClient_LoopbackHTTPAlwaysPermitted(t *testing.T) {
	t.Setenv("ENTIRE_AUTH_BASE_URL", "http://127.0.0.1:8787")
	c := NewClient(nil, false)
	if !c.inner.AllowInsecureHTTP {
		t.Fatal("NewClient(nil, false) AllowInsecureHTTP = false for loopback, want true")
	}
}

func TestIsLoopbackHTTP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"http://localhost:8080", true},
		{"http://127.0.0.1", true},
		{"http://[::1]:8080", true},
		{"https://localhost", false},     // https never qualifies
		{"http://entire.io", false},      // public host
		{"http://[::1]:8080/path", true}, // path doesn't matter for the host check
		{"", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHTTP(tc.in); got != tc.want {
			t.Errorf("isLoopbackHTTP(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
