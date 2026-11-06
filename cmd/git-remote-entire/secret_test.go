package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// testSASecret is Core's v1 test vector; see cmd/entire/cli/auth/env_secret_test.go.
const testSASecret = "ent_svc_us_01JABCDEFGHJKMNPQRSTVWXYZ0_01JZYXWVTSRQPNMKJHGFEDCBA9_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq3MriFT"

// makeSecretJWT is a service account token as Core issues it for testSASecret:
// for its account, with core as its only audience, expiring in 10 minutes.
// suffix varies the signature so successive tokens differ.
func makeSecretJWT(t *testing.T, core, suffix string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString(fmt.Appendf(nil, `{"sub":"svc_01JABCDEFGHJKMNPQRSTVWXYZ0","aud":%q,"exp":%d}`, core, time.Now().Add(10*time.Minute).Unix()))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"+suffix))
}

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

// useSecretEnv isolates the cache and sets ENTIRE_TOKEN to a secret. The
// exchange goes to the secret's jurisdiction Core, https://us.auth.entire.io.
// Not parallel-safe: it sets process-wide environment and the seam.
func useSecretEnv(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv(auth.EnvTokenVar, testSASecret)
	t.Cleanup(auth.UseSecretExchangeTransportForTesting(rt))
}

// The helper exchanges a secret and sends the service account JWT to the
// data plane, never the secret. Not parallel: sets process-wide environment.
func TestResolveCreds_ServiceAccountSecretUsesExchangedJWT(t *testing.T) {
	const core = "https://us.auth.entire.io"
	jwt := makeSecretJWT(t, core, "")
	useSecretEnv(t, secretExchangeFunc(func(r *http.Request) (*http.Response, error) {
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
	useSecretEnv(t, secretExchangeFunc(func(*http.Request) (*http.Response, error) {
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

// A 401 drops the cached token, so the transport's retry exchanges the secret
// again instead of resending the rejected token. Without a 401, requests reuse
// the cached token. Not parallel: sets process-wide environment.
func TestResolveCreds_ServiceAccountSecretReexchangesAfter401(t *testing.T) {
	const core = "https://us.auth.entire.io"
	exchanges := 0
	useSecretEnv(t, secretExchangeFunc(func(*http.Request) (*http.Response, error) {
		exchanges++
		jwt := makeSecretJWT(t, core, strconv.Itoa(exchanges))
		return secretReply(http.StatusOK, fmt.Sprintf(`{"access_token":%q,"expires_in":3600}`, jwt)), nil
	}))
	client, clusterHost := wellKnownServer(t, []string{core}, "", "")

	creds, onUnauthorized, err := resolveCreds(t.Context(), &url.URL{Scheme: "entire", Host: clusterHost}, false, client)
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	first, err := creds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	again, err := creds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again != first || exchanges != 1 {
		t.Fatalf("second request: token changed or %d exchanges; want the cached token", exchanges)
	}

	onUnauthorized()
	retried, err := creds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if retried == first || exchanges != 2 {
		t.Errorf("after a 401: %d exchanges, token unchanged=%v; want a fresh exchange", exchanges, retried == first)
	}
}

// A 401 for a token another process already replaced keeps the replacement:
// the retry reuses it instead of exchanging again. Not parallel: sets
// process-wide environment.
func TestResolveCreds_ServiceAccountSecret401KeepsReplacement(t *testing.T) {
	const core = "https://us.auth.entire.io"
	exchanges := 0
	useSecretEnv(t, secretExchangeFunc(func(*http.Request) (*http.Response, error) {
		exchanges++
		jwt := makeSecretJWT(t, core, strconv.Itoa(exchanges))
		return secretReply(http.StatusOK, fmt.Sprintf(`{"access_token":%q,"expires_in":3600}`, jwt)), nil
	}))
	client, clusterHost := wellKnownServer(t, []string{core}, "", "")

	creds, onUnauthorized, err := resolveCreds(t.Context(), &url.URL{Scheme: "entire", Host: clusterHost}, false, client)
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	first, err := creds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Another process drops the cached token and exchanges a replacement.
	auth.ForgetServiceAccountSecretToken(testSASecret)
	replacement, err := auth.ResolveEnvToken(testSASecret)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == first || exchanges != 2 {
		t.Fatalf("setup: %d exchanges, replacement unchanged=%v", exchanges, replacement == first)
	}

	onUnauthorized() // the 401 was for first, not the replacement
	retried, err := creds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if retried != replacement || exchanges != 2 {
		t.Errorf("after a 401 for the replaced token: %d exchanges, got replacement=%v; want the replacement reused", exchanges, retried == replacement)
	}
}
