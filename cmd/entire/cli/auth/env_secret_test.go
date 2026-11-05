package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testServiceAccountSecret = "ent_svc_01JABCDEFGHJKMNPQRSTVWXYZ0_01JABCDEFGHJKMNPQRSTVWXYZ1_abcdefghijklmnop"

// fakeSecretCore answers /oauth/token like a Core and counts exchanges.
type fakeSecretCore struct {
	t         *testing.T
	exchanges atomic.Int32
	status    int
	jwt       string
	expiresIn int
	lastForm  url.Values
}

func (f *fakeSecretCore) RoundTrip(r *http.Request) (*http.Response, error) {
	// A real transport honours the request context; so does the fake.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	f.exchanges.Add(1)
	if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
	}
	if user, _, ok := r.BasicAuth(); !ok || user != "entire-cli" {
		f.t.Errorf("basic auth user = %q", user)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if f.lastForm, err = url.ParseQuery(string(body)); err != nil {
		return nil, err
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	payload := fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":%d}`, f.jwt, f.expiresIn)
	if status != http.StatusOK {
		payload = `{"error":"invalid_grant","error_description":"secret refused"}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    r,
	}, nil
}

// useFakeSecretCore isolates the cache and routes exchanges to a fake Core.
// Not parallel-safe: it sets process-wide environment and seams.
func useFakeSecretCore(t *testing.T, core *fakeSecretCore) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv(SecretCoreURLVar, "https://core.example.com")
	t.Cleanup(UseSecretExchangeTransportForTesting(core))
}

// Not parallel: sets process-wide environment.
func TestResolveEnvTokenPassesThroughNonSecrets(t *testing.T) {
	core := &fakeSecretCore{t: t}
	useFakeSecretCore(t, core)

	got, err := ResolveEnvToken("  a.jwt.token \n")
	if err != nil || got != "a.jwt.token" {
		t.Fatalf("ResolveEnvToken = %q, %v", got, err)
	}
	if _, err := ResolveEnvToken("   "); err == nil {
		t.Error("a blank token must fail closed")
	}
	if n := core.exchanges.Load(); n != 0 {
		t.Errorf("exchanges = %d, want 0", n)
	}
}

// Not parallel: sets process-wide environment.
func TestParseEnvTokenExchangesSecret(t *testing.T) {
	jwt := makeJWT(t, `{"iss":"https://core.example.com","aud":"https://core.example.com","sub":"svc_01JABCDEFGHJKMNPQRSTVWXYZ0"}`)
	core := &fakeSecretCore{t: t, jwt: jwt, expiresIn: 600}
	useFakeSecretCore(t, core)

	coreURL, token, err := ParseEnvToken(testServiceAccountSecret)
	if err != nil {
		t.Fatalf("ParseEnvToken: %v", err)
	}
	if token != jwt || coreURL != "https://core.example.com" {
		t.Errorf("ParseEnvToken = %q, %q", coreURL, token)
	}
	want := map[string]string{
		"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"subject_token":      testServiceAccountSecret,
		"audience":           "https://core.example.com",
		"resource":           "svc_01JABCDEFGHJKMNPQRSTVWXYZ0",
	}
	for k, v := range want {
		if got := core.lastForm.Get(k); got != v {
			t.Errorf("form %s = %q, want %q", k, got, v)
		}
	}

	// A second command reuses the cached token.
	if _, _, err := ParseEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 1 {
		t.Errorf("exchanges = %d, want 1 (cached)", n)
	}
}

// Not parallel: sets process-wide environment and the clock seam.
func TestSecretTokenCacheExpires(t *testing.T) {
	core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 600}
	useFakeSecretCore(t, core)
	now := time.Now()
	prev := secretNow
	secretNow = func() time.Time { return now }
	t.Cleanup(func() { secretNow = prev })

	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	now = now.Add(600*time.Second - secretTokenMargin + time.Second)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (renewed near expiry)", n)
	}
}

// Not parallel: sets process-wide environment.
func TestSecretExchangeRefusalHidesSecret(t *testing.T) {
	core := &fakeSecretCore{t: t, status: http.StatusBadRequest}
	useFakeSecretCore(t, core)

	_, err := ResolveEnvToken(testServiceAccountSecret)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), testServiceAccountSecret) {
		t.Errorf("error leaks the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %v, want the OAuth error", err)
	}
}

// Not parallel: sets process-wide environment.
func TestSecretCoreURLValidation(t *testing.T) {
	core := &fakeSecretCore{t: t}
	useFakeSecretCore(t, core)

	t.Setenv(SecretCoreURLVar, "http://core.example.com")
	if _, err := ResolveEnvToken(testServiceAccountSecret); err == nil {
		t.Error("an http Core must be refused: the secret would travel in cleartext")
	}
	t.Setenv(SecretCoreURLVar, "")
	if got, err := secretCoreURL(); err != nil || got != defaultSecretCoreURL {
		t.Errorf("default Core = %q, %v", got, err)
	}
	if _, err := ResolveEnvToken("ent_svc_malformed"); err == nil {
		t.Error("a malformed secret must fail")
	}
	if n := core.exchanges.Load(); n != 0 {
		t.Errorf("exchanges = %d, want 0", n)
	}
}

// The cache key for a secret names the Core and the service account, never
// the secret, and computing it makes no exchange.
// Not parallel: sets process-wide environment.
func TestLocalIdentityCacheKeyForSecret(t *testing.T) {
	core := &fakeSecretCore{t: t}
	useFakeSecretCore(t, core)
	t.Setenv(EnvTokenVar, testServiceAccountSecret)

	got, err := LocalIdentityCacheKey()
	if err != nil {
		t.Fatalf("LocalIdentityCacheKey: %v", err)
	}
	if want := "env-sa|https://core.example.com|svc_01JABCDEFGHJKMNPQRSTVWXYZ0"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if strings.Contains(got, "abcdefghijklmnop") {
		t.Error("the key contains the secret")
	}
	if n := core.exchanges.Load(); n != 0 {
		t.Errorf("exchanges = %d, want 0", n)
	}
}

// ctx cancels an exchange in flight.
// Not parallel: sets process-wide environment.
func TestResolveEnvTokenContextHonorsCancel(t *testing.T) {
	core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 600}
	useFakeSecretCore(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResolveEnvTokenContext(ctx, testServiceAccountSecret); err == nil {
		t.Error("a cancelled context must stop the exchange")
	}
}
