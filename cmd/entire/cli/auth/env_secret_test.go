package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// testServiceAccountSecret is Core's v1 test vector (TestSecretFormatVector
// in core/serviceaccount/secrets_test.go): jurisdiction us, account
// svc_01JABCDEFGHJKMNPQRSTVWXYZ0, secret sas_01JZYXWVTSRQPNMKJHGFEDCBA9,
// random bytes 0x00..0x1f. Every client must parse it.
const testServiceAccountSecret = "ent_svc_us_01JABCDEFGHJKMNPQRSTVWXYZ0_01JZYXWVTSRQPNMKJHGFEDCBA9_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq3MriFT"

// testServiceAccountSecretEU is the same inputs in jurisdiction eu, checksum
// computed independently with Python's zlib.crc32.
const testServiceAccountSecretEU = "ent_svc_eu_01JABCDEFGHJKMNPQRSTVWXYZ0_01JZYXWVTSRQPNMKJHGFEDCBA9_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq3BaGLv"

// fakeSecretCore answers /oauth/token like a Core and counts exchanges. With
// jwt unset it mints a token for the requested account and audience that
// expires after expiresIn on the secretNow clock.
type fakeSecretCore struct {
	t         *testing.T
	exchanges atomic.Int32
	status    int
	jwt       string
	expiresIn int
	lastForm  url.Values
	// during runs inside the exchange, while the exchange lock is held.
	during func()
}

func secretTokenJWT(t *testing.T, sub, aud string, exp time.Time) string {
	t.Helper()
	return makeJWT(t, fmt.Sprintf(`{"sub":%q,"aud":%q,"exp":%d}`, sub, aud, exp.Unix()))
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
	if f.during != nil {
		f.during()
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	jwt := f.jwt
	if jwt == "" {
		exp := secretNow().Add(time.Duration(f.expiresIn) * time.Second)
		jwt = secretTokenJWT(f.t, f.lastForm.Get("resource"), f.lastForm.Get("audience"), exp)
	}
	payload := fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":%d}`, jwt, f.expiresIn)
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
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":"https://us.auth.entire.io","aud":"https://us.auth.entire.io","sub":"svc_01JABCDEFGHJKMNPQRSTVWXYZ0","exp":%d}`, time.Now().Add(10*time.Minute).Unix()))
	core := &fakeSecretCore{t: t, jwt: jwt, expiresIn: 600}
	useFakeSecretCore(t, core)

	coreURL, token, err := ParseEnvToken(testServiceAccountSecret)
	if err != nil {
		t.Fatalf("ParseEnvToken: %v", err)
	}
	if token != jwt || coreURL != "https://us.auth.entire.io" {
		t.Errorf("ParseEnvToken = %q, %q", coreURL, token)
	}
	want := map[string]string{
		"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"subject_token":      testServiceAccountSecret,
		"audience":           "https://us.auth.entire.io",
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
	core := &fakeSecretCore{t: t, expiresIn: 600}
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
func TestMalformedSecretNeverExchanged(t *testing.T) {
	core := &fakeSecretCore{t: t}
	useFakeSecretCore(t, core)

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
	if want := "env-sa|https://us.auth.entire.io|svc_01JABCDEFGHJKMNPQRSTVWXYZ0"; got != want {
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
	core := &fakeSecretCore{t: t, expiresIn: 600}
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
	core := &fakeSecretCore{t: t, expiresIn: 3600}
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
	core := &fakeSecretCore{t: t, expiresIn: 600}
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

// The secret's jurisdiction picks the Core; the
// exchange, the identity cache key and Forget all use that origin.
// Not parallel: sets process-wide environment.
func TestSecretCoreDerivedFromJurisdiction(t *testing.T) {
	for _, tc := range []struct{ secret, core string }{
		{testServiceAccountSecret, "https://us.auth.entire.io"},
		{testServiceAccountSecretEU, "https://eu.auth.entire.io"},
	} {
		t.Run(tc.core, func(t *testing.T) {
			core := &fakeSecretCore{t: t, expiresIn: 600}
			useFakeSecretCore(t, core)
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

// An exchanged token that is not for this account at this Core, or carries no
// usable exp, is refused and never cached.
// Not parallel: sets process-wide environment.
func TestSecretExchangeRejectsUnusableToken(t *testing.T) {
	const (
		account = "svc_01JABCDEFGHJKMNPQRSTVWXYZ0"
		core    = "https://us.auth.entire.io"
	)
	exp := time.Now().Add(10 * time.Minute)
	for _, tc := range []struct{ name, jwt string }{
		{"not a JWT", "opaque-token"},
		{"other account", secretTokenJWT(t, "svc_01JZYXWVTSRQPNMKJHGFEDCBA9", core, exp)},
		{"other origin", secretTokenJWT(t, account, "https://evil.example.com", exp)},
		{"two origins", makeJWT(t, fmt.Sprintf(`{"sub":%q,"aud":[%q,"https://evil.example.com"],"exp":%d}`, account, core, exp.Unix()))},
		{"no exp", makeJWT(t, fmt.Sprintf(`{"sub":%q,"aud":%q}`, account, core))},
		{"expired", secretTokenJWT(t, account, core, time.Now().Add(-time.Minute))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSecretCore{t: t, jwt: tc.jwt, expiresIn: 600}
			useFakeSecretCore(t, fake)
			for range 2 {
				_, err := ResolveEnvToken(testServiceAccountSecret)
				if err == nil || !strings.Contains(err.Error(), "unusable service account token") {
					t.Fatalf("err = %v, want an unusable-token refusal", err)
				}
			}
			if n := fake.exchanges.Load(); n != 2 {
				t.Errorf("exchanges = %d, want 2 (nothing cached)", n)
			}
		})
	}
}

// The token's own exp bounds reuse when it is earlier than expires_in.
// Not parallel: sets process-wide environment and the clock seam.
func TestSecretTokenCacheUsesEarlierJWTExp(t *testing.T) {
	start := time.Now()
	now := start
	prev := secretNow
	secretNow = func() time.Time { return now }
	t.Cleanup(func() { secretNow = prev })
	fake := &fakeSecretCore{t: t, expiresIn: 600,
		jwt: secretTokenJWT(t, "svc_01JABCDEFGHJKMNPQRSTVWXYZ0", "https://us.auth.entire.io", start.Add(5*time.Minute))}
	useFakeSecretCore(t, fake)

	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	// Within expires_in's window, but past the JWT's exp minus the margin.
	now = start.Add(5*time.Minute - secretTokenMargin + time.Second)
	fake.jwt = ""
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if n := fake.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (the JWT's exp ends reuse)", n)
	}
}

// A cache entry whose token fails the claim checks is a miss.
// Not parallel: sets process-wide environment.
func TestSecretTokenCacheRechecksClaims(t *testing.T) {
	fake := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, fake)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	subj, err := secretExchangeSubject(testServiceAccountSecret)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := secretCacheRoot()
	if !ok {
		t.Fatal("no cache root")
	}
	planted := secretTokenJWT(t, "svc_01JZYXWVTSRQPNMKJHGFEDCBA9", subj.core, time.Now().Add(10*time.Minute))
	writeCachedSecretToken(root, subj, planted, time.Now().Add(10*time.Minute))

	got, err := ResolveEnvToken(testServiceAccountSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got == planted || fake.exchanges.Load() != 2 {
		t.Errorf("exchanges = %d, reused planted token = %v; want a fresh exchange", fake.exchanges.Load(), got == planted)
	}
}

// The exchange never follows a redirect: a 307/308 would resend the body,
// which holds the secret, to the new host.
// Not parallel: sets process-wide environment.
func TestSecretExchangeDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", home)
			var other atomic.Int32
			t.Cleanup(UseSecretExchangeTransportForTesting(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "us.auth.entire.io" {
					other.Add(1)
					body, _ := io.ReadAll(r.Body) //nolint:errcheck // only inspected for the secret
					if strings.Contains(string(body), "ent_svc_") {
						t.Error("the redirect target received the secret")
					}
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Location": {"https://other.example.com/oauth/token"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			})))

			_, err := ResolveEnvToken(testServiceAccountSecret)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
				t.Errorf("err = %v, want the %d reported as a refusal", err, status)
			}
			if n := other.Load(); n != 0 {
				t.Errorf("the redirect target saw %d requests, want 0", n)
			}
		})
	}
}

// tryLockSecretCache reports whether the subject's exchange lock is free now.
func tryLockSecretCache(t *testing.T, subj exchangeSubject) bool {
	t.Helper()
	root, ok := secretCacheRoot()
	if !ok {
		t.Fatal("no cache root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	release, err := lockSecretCache(ctx, root, subj)
	if err != nil {
		return false
	}
	release()
	return true
}

// The exchange runs under the per-subject lock, so a parallel process on a
// cold cache waits for it and then reads its result.
// Not parallel: sets process-wide environment.
func TestSecretExchangeHoldsLock(t *testing.T) {
	var subj exchangeSubject
	fake := &fakeSecretCore{t: t, expiresIn: 600}
	fake.during = func() {
		if tryLockSecretCache(t, subj) {
			t.Error("the exchange lock was free during the exchange")
		}
	}
	useFakeSecretCore(t, fake)
	subj, err := secretExchangeSubject(testServiceAccountSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	if !tryLockSecretCache(t, subj) {
		t.Error("the exchange lock is still held after the exchange")
	}
}

// Forget waits for the exchange lock, so it cannot interleave with another
// process's exchange and write-back.
// Not parallel: sets process-wide environment.
func TestForgetServiceAccountSecretTokenTakesLock(t *testing.T) {
	fake := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, fake)
	if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
		t.Fatal(err)
	}
	subj, err := secretExchangeSubject(testServiceAccountSecret)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := secretCacheRoot()
	if !ok {
		t.Fatal("no cache root")
	}
	release, err := lockSecretCache(context.Background(), root, subj)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		ForgetServiceAccountSecretToken(testServiceAccountSecret)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Forget returned while another holder had the lock")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := osroot.ReadFileNoFollow(root, secretCacheFile(subj)); err != nil {
		t.Errorf("Forget removed the entry without the lock: %v", err)
	}
	release()
	<-done
	if _, err := osroot.ReadFileNoFollow(root, secretCacheFile(subj)); err == nil {
		t.Error("the entry survived Forget")
	}
}

// A lock that cannot be taken for a reason other than cancellation costs
// sharing, not auth: the exchange still runs, and nothing is cached.
// Not parallel: sets process-wide environment.
func TestSecretExchangeFallsBackWhenLockFails(t *testing.T) {
	fake := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, fake)
	subj, err := secretExchangeSubject(testServiceAccountSecret)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := secretCacheRoot()
	if !ok {
		t.Fatal("no cache root")
	}
	// A directory where the lock file belongs makes opening it fail.
	if err := root.Mkdir(secretCacheFile(subj)+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := ResolveEnvToken(testServiceAccountSecret); err != nil {
			t.Fatalf("ResolveEnvToken with a broken lock: %v", err)
		}
	}
	if n := fake.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (unshared, uncached)", n)
	}
}
