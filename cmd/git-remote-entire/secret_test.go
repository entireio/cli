package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

const testSASecret = "ent_svc_01JABCDEFGHJKMNPQRSTVWXYZ0_01JABCDEFGHJKMNPQRSTVWXYZ1_abcdefghijklmnop"

// secretExchangeFunc answers /oauth/token for the secret exchange.
type secretExchangeFunc func(*http.Request) (*http.Response, error)

func (f secretExchangeFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func secretReply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// useSecretEnv isolates the cache and sets ENTIRE_TOKEN to a secret.
// Not parallel-safe: it sets process-wide environment and the seam.
func useSecretEnv(t *testing.T, core string, rt http.RoundTripper) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv(auth.SecretCoreURLVar, core)
	t.Setenv(auth.EnvTokenVar, testSASecret)
	t.Cleanup(auth.UseSecretExchangeTransportForTesting(rt))
}

// The helper exchanges a secret and sends the service account JWT to the
// data plane, never the secret. Not parallel: sets process-wide environment.
func TestResolveCreds_ServiceAccountSecretUsesExchangedJWT(t *testing.T) {
	const core = "https://core.us.entire.io"
	jwt := makeTestJWT(t, core)
	useSecretEnv(t, core, secretExchangeFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != core+"/oauth/token" {
			t.Errorf("exchange went to %s", r.URL)
		}
		return secretReply(http.StatusOK, fmt.Sprintf(`{"access_token":%q,"expires_in":600}`, jwt)), nil
	}))
	client, clusterHost := wellKnownServer(t, []string{core}, "", "")

	creds, _, err := resolveCreds(t.Context(), &url.URL{Scheme: "entire", Host: clusterHost}, false, client)
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	got, err := creds(t.Context())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != jwt {
		t.Errorf("bearer = %q, want the exchanged JWT", got)
	}
}

// A refused secret fails before discovery and never names the secret.
// Not parallel: sets process-wide environment.
func TestResolveCreds_RefusedServiceAccountSecretFailsClosed(t *testing.T) {
	useSecretEnv(t, "https://core.us.entire.io", secretExchangeFunc(func(*http.Request) (*http.Response, error) {
		return secretReply(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refused"}`), nil
	}))
	// A nil client panics if discovery runs.
	creds, _, err := resolveCreds(t.Context(), &url.URL{Scheme: "entire", Host: "cluster.example.com"}, false, nil)
	if err == nil || creds != nil {
		t.Fatalf("resolveCreds = %v, %v; want an error and no creds", creds, err)
	}
	if strings.Contains(err.Error(), testSASecret) {
		t.Errorf("error leaks the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %v, want the OAuth error", err)
	}
}
