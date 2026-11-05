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

// testServiceAccountSecret is Core's v1 test vector (TestSecretFormatVector
// in core/serviceaccount/secrets_test.go): jurisdiction us, account
// svc_01JABCDEFGHJKMNPQRSTVWXYZ0, secret sas_01JZYXWVTSRQPNMKJHGFEDCBA9,
// random bytes 0x00..0x1f. Every client must parse it.
const testServiceAccountSecret = "ent_svc_us_01JABCDEFGHJKMNPQRSTVWXYZ0_01JZYXWVTSRQPNMKJHGFEDCBA9_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq3MriFT"

// testServiceAccountSecretEU is the same inputs in jurisdiction eu, checksum
// computed independently with Python's zlib.crc32.
const testServiceAccountSecretEU = "ent_svc_eu_01JABCDEFGHJKMNPQRSTVWXYZ0_01JZYXWVTSRQPNMKJHGFEDCBA9_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq3BaGLv"

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

// Core issues tokens that live at most 10 minutes. A 600-second token is
// reused until secretTokenMargin before it expires, then exchanged again.
// Not parallel: sets process-wide environment and the clock seam.
func TestSecretTokenCacheReusesTenMinuteTokenUntilMargin(t *testing.T) {
	core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 600}
	useFakeSecretCore(t, core)
	start := time.Now()
	now := start
	prev := secretNow
	secretNow = func() time.Time { return now }
	t.Cleanup(func() { secretNow = prev })

	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	now = start.Add(600*time.Second - secretTokenMargin - time.Second)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 1 {
		t.Fatalf("exchanges = %d just inside the margin, want 1 (cached)", n)
	}
	now = start.Add(600*time.Second - secretTokenMargin + time.Second)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d past the margin, want 2 (renewed near expiry)", n)
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
	t.Setenv(SecretCoreURLVar, "https://user:pw@core.example.com")
	if _, err := ResolveEnvToken(testServiceAccountSecret); err == nil {
		t.Error("a Core URL with userinfo must be refused")
	}
	t.Setenv(SecretCoreURLVar, "")
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
	if strings.Contains(got, "aaaqeayeaudaocaj") {
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

// A long-lived token is still reused for at most secretCacheMaxAge, so a token
// Core invalidates early is not reused for its whole lifetime.
// Not parallel: sets process-wide environment and the clock seam.
func TestSecretTokenCacheMaxAge(t *testing.T) {
	core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 3600}
	useFakeSecretCore(t, core)
	now := time.Now()
	prev := secretNow
	secretNow = func() time.Time { return now }
	t.Cleanup(func() { secretNow = prev })

	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	now = now.Add(secretCacheMaxAge - time.Second)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 1 {
		t.Fatalf("exchanges = %d inside the window, want 1", n)
	}
	now = now.Add(2 * time.Second)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d after the window, want 2", n)
	}
}

// Forgetting a secret's token forces the next resolve to exchange.
// Not parallel: sets process-wide environment.
func TestForgetServiceAccountSecretToken(t *testing.T) {
	core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 600}
	useFakeSecretCore(t, core)
	for range 2 {
		if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
			t.Fatal(err)
		}
	}
	ForgetServiceAccountSecretToken(testServiceAccountSecret)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := core.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (one cached reuse, one after forget)", n)
	}
}

func TestParseServiceAccountSecretV1(t *testing.T) {
	t.Parallel()
	if got := secretChecksum(testServiceAccountSecret[:len(testServiceAccountSecret)-secretCheckLen]); got != "3MriFT" {
		t.Errorf("vector checksum = %q, want 3MriFT", got)
	}
	for _, tc := range []struct{ secret, juri string }{
		{testServiceAccountSecret, "us"},
		{testServiceAccountSecretEU, "eu"},
	} {
		got, err := parseServiceAccountSecret(tc.secret)
		if err != nil {
			t.Fatalf("parse %s vector: %v", tc.juri, err)
		}
		want := serviceAccountSecret{jurisdiction: tc.juri, accountID: "svc_01JABCDEFGHJKMNPQRSTVWXYZ0"}
		if got != want {
			t.Errorf("parse %s vector = %+v, want %+v", tc.juri, got, want)
		}
	}
}

// Each refusal names its reason and never the secret.
func TestParseServiceAccountSecretRefusals(t *testing.T) {
	t.Parallel()
	const (
		account = "01JABCDEFGHJKMNPQRSTVWXYZ0"
		secret  = "01JZYXWVTSRQPNMKJHGFEDCBA9"
		random  = "aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq"
	)
	// withCheck appends a valid checksum, so only the intended rule fails.
	withCheck := func(body string) string { return body + secretChecksum(body) }
	for _, tc := range []struct {
		name, secret, reason string
	}{
		{"old 3-part layout", "ent_svc_" + account + "_" + secret + "_abcdefghijklmnop", "want ent_svc_"},
		{"empty jurisdiction", withCheck("ent_svc__" + account + "_" + secret + "_" + random), "jurisdiction"},
		{"uppercase jurisdiction", withCheck("ent_svc_US_" + account + "_" + secret + "_" + random), "jurisdiction"},
		{"long jurisdiction", withCheck("ent_svc_abcdefghi_" + account + "_" + secret + "_" + random), "jurisdiction"},
		{"digit in jurisdiction", withCheck("ent_svc_u1_" + account + "_" + secret + "_" + random), "jurisdiction"},
		{"bad account ULID", withCheck("ent_svc_us_01JABCDEFGHJKMNPQRSTVWXYZ_" + secret + "_" + random), "service account ID"},
		{"lowercase account ULID", withCheck("ent_svc_us_" + strings.ToLower(account) + "_" + secret + "_" + random), "service account ID"},
		{"overflowing account ULID", withCheck("ent_svc_us_81JABCDEFGHJKMNPQRSTVWXYZ0_" + secret + "_" + random), "service account ID"},
		{"bad secret ULID", withCheck("ent_svc_us_" + account + "_01JZYXWVTSRQPNMKJHGFEDCBAI_" + random), "secret ID"},
		{"lowercase secret ULID", withCheck("ent_svc_us_" + account + "_" + strings.ToLower(secret) + "_" + random), "secret ID"},
		{"random too short", withCheck("ent_svc_us_" + account + "_" + secret + "_" + random[1:]), "wrong length"},
		{"random too long", withCheck("ent_svc_us_" + account + "_" + secret + "_" + random + "a"), "wrong length"},
		{"bad base32 character", withCheck("ent_svc_us_" + account + "_" + secret + "_" + "1" + random[1:]), "base32"},
		{"uppercase base32", withCheck("ent_svc_us_" + account + "_" + secret + "_" + "A" + random[1:]), "base32"},
		// The vector's random part ends in "q" (10000); "r" (10001) decodes to
		// the same bytes but sets an unused bit.
		{"unused bits set", withCheck("ent_svc_us_" + account + "_" + secret + "_" + random[:len(random)-1] + "r"), "unused bits"},
		{"checksum mismatch", testServiceAccountSecret[:len(testServiceAccountSecret)-1] + "5", "checksum mismatch"},
		{"one-character typo", strings.Replace(testServiceAccountSecret, "aaaq", "aaab", 1), "checksum mismatch"},
		{"truncated", testServiceAccountSecret[:len(testServiceAccountSecret)-1], "wrong length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseServiceAccountSecret(tc.secret)
			if err == nil {
				t.Fatal("want an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "malformed service account secret") || !strings.Contains(msg, tc.reason) {
				t.Errorf("error = %q, want a malformed-secret error naming %q", msg, tc.reason)
			}
			if strings.Contains(msg, random[:16]) || strings.Contains(msg, tc.secret) {
				t.Errorf("error leaks the secret: %q", msg)
			}
		})
	}
}

// With ENTIRE_CORE_URL unset, the secret's jurisdiction picks the Core; the
// exchange, the identity cache key and Forget all use that origin.
// Not parallel: sets process-wide environment.
func TestSecretCoreDerivedFromJurisdiction(t *testing.T) {
	for _, tc := range []struct{ secret, core string }{
		{testServiceAccountSecret, "https://us.auth.entire.io"},
		{testServiceAccountSecretEU, "https://eu.auth.entire.io"},
	} {
		t.Run(tc.core, func(t *testing.T) {
			core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 600}
			useFakeSecretCore(t, core)
			t.Setenv(SecretCoreURLVar, "")
			t.Setenv(EnvTokenVar, tc.secret)

			key, err := LocalIdentityCacheKey()
			if err != nil {
				t.Fatal(err)
			}
			if want := "env-sa|" + tc.core + "|svc_01JABCDEFGHJKMNPQRSTVWXYZ0"; key != want {
				t.Errorf("key = %q, want %q", key, want)
			}
			for range 2 {
				if _, err := ResolveEnvToken(tc.secret); err != nil {
					t.Fatal(err)
				}
			}
			if got := core.lastForm.Get("audience"); got != tc.core {
				t.Errorf("audience = %q, want %q", got, tc.core)
			}
			ForgetServiceAccountSecretToken(tc.secret)
			if _, err := ResolveEnvToken(tc.secret); err != nil {
				t.Fatal(err)
			}
			if n := core.exchanges.Load(); n != 2 {
				t.Errorf("exchanges = %d, want 2 (one cached reuse, one after forget)", n)
			}
		})
	}
}

// ENTIRE_CORE_URL wins over the jurisdiction, and is not checked against it.
// Not parallel: sets process-wide environment.
func TestSecretCoreOverrideWins(t *testing.T) {
	core := &fakeSecretCore{t: t, jwt: "jwt", expiresIn: 600}
	useFakeSecretCore(t, core)
	t.Setenv(SecretCoreURLVar, "https://core.staging.example.com/ignored/path")

	if _, err := ResolveEnvToken(testServiceAccountSecretEU); err != nil {
		t.Fatal(err)
	}
	if got := core.lastForm.Get("audience"); got != "https://core.staging.example.com" {
		t.Errorf("audience = %q, want the override origin", got)
	}
}
