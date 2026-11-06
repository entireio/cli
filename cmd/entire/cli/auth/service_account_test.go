package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive process-global state (ENTIRE_CONFIG_DIR, XDG_CACHE_HOME,
// ENTIRE_TOKEN, ENTIRE_SERVICE_ACCOUNT, nowFunc) so they cannot run in parallel.

const testServiceAccount = "svc_01HKZ3J4N5P6Q7R8S9T0V1W2X3"

func isolateServiceAccountDirs(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, name := range CredentialEnvVars() {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return configDir
}

func newTestKey(t *testing.T, server string) *ServiceAccountKey {
	t.Helper()
	key, err := GenerateServiceAccountKey(testServiceAccount, server, "box-1", nowFunc())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestParseServiceAccountID(t *testing.T) {
	t.Parallel()
	got, err := ParseServiceAccountID(" svc_01hkz3j4n5p6q7r8s9t0v1w2x3 ")
	if err != nil || got != testServiceAccount {
		t.Fatalf("ParseServiceAccountID(lowercase) = %q, %v; want %q", got, err, testServiceAccount)
	}
	for _, bad := range []string{"", "01HKZ3J4N5P6Q7R8S9T0V1W2X3", "svc_", "svc_../../etc/passwd", "svc_01HKZ3J4N5P6Q7R8S9T0V1W2X3/x"} {
		if _, err := ParseServiceAccountID(bad); err == nil {
			t.Errorf("ParseServiceAccountID(%q) succeeded, want error", bad)
		}
	}
}

func TestCreateServiceAccountKey_FirstActivatesLaterStayInactive(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	ctx := context.Background()

	first := newTestKey(t, "https://us.auth.example.com/")
	if first.Server() != "https://us.auth.example.com" {
		t.Fatalf("server = %q, want bare origin", first.Server())
	}
	if activated, err := CreateServiceAccountKey(ctx, first); err != nil || !activated {
		t.Fatalf("first key: activated %v err %v; want it active", activated, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(configDir, ServiceAccountKeyFileName(first.KeyID())))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("key file perm = %o, want 600", perm)
		}
	}

	second := newTestKey(t, "https://us.auth.example.com")
	if activated, err := CreateServiceAccountKey(ctx, second); err != nil || activated {
		t.Fatalf("second key: activated %v err %v; an existing active key must not be displaced", activated, err)
	}
	active, err := LoadActiveServiceAccountKey(testServiceAccount)
	if err != nil || active.KeyID() != first.KeyID() {
		t.Fatalf("active key = %v, %v; want the first key", active, err)
	}
	wantPub, _ := first.PublicKeyPEM() //nolint:errcheck // compared below
	gotPub, _ := active.PublicKeyPEM() //nolint:errcheck // compared below
	if gotPub != wantPub {
		t.Fatal("loaded key has a different public key")
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{first.KeyID(): KeyActive, second.KeyID(): KeyInactive})
}

func assertKeyStatuses(t *testing.T, want map[string]ServiceAccountKeyStatus) {
	t.Helper()
	keys, err := ListServiceAccountKeys("")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ServiceAccountKeyStatus{}
	for _, k := range keys {
		got[k.Key.KeyID()] = k.Status
	}
	if len(got) != len(want) {
		t.Fatalf("stored keys = %v, want %v", got, want)
	}
	for id, status := range want {
		if got[id] != status {
			t.Fatalf("key %s status = %q, want %q (all: %v)", id, got[id], status, got)
		}
	}
}

// The exclusive create must never replace a stored key, even the same one.
func TestCreateServiceAccountKey_NeverOverwrites(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	key := newTestKey(t, "https://us.auth.example.com")
	if _, err := CreateServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, ServiceAccountKeyFileName(key.KeyID()))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateServiceAccountKey(context.Background(), key); !errors.Is(err, ErrServiceAccountKeyExists) {
		t.Fatalf("second create: err = %v, want ErrServiceAccountKeyExists", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("key file changed by a refused create (err %v)", err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

// Concurrent creates for one account: every key is stored and exactly one is
// active. Before the lock, check-then-rename let one write replace another.
func TestCreateServiceAccountKey_ConcurrentCreatesKeepEveryKey(t *testing.T) {
	isolateServiceAccountDirs(t)
	const n = 8
	keys := make([]*ServiceAccountKey, n)
	for i := range keys {
		keys[i] = newTestKey(t, "https://us.auth.example.com")
	}
	var wg sync.WaitGroup
	var activated atomic.Int32
	errs := make(chan error, n)
	for _, k := range keys {
		wg.Go(func() {
			ok, err := CreateServiceAccountKey(context.Background(), k)
			if err != nil {
				errs <- err
			}
			if ok {
				activated.Add(1)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := activated.Load(); got != 1 {
		t.Fatalf("%d keys activated, want exactly 1", got)
	}
	stored, err := ListServiceAccountKeys(testServiceAccount)
	if err != nil || len(stored) != n {
		t.Fatalf("stored %d keys (err %v), want %d", len(stored), err, n)
	}
}

func TestActivateServiceAccountKey_SwitchesAndRetiresPrevious(t *testing.T) {
	isolateServiceAccountDirs(t)
	ctx := context.Background()
	oldKey := newTestKey(t, "https://us.auth.example.com")
	newKey := newTestKey(t, "https://us.auth.example.com")
	for _, k := range []*ServiceAccountKey{oldKey, newKey} {
		if _, err := CreateServiceAccountKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}

	retiring, err := ActivateServiceAccountKey(ctx, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(retiring) != 1 || retiring[0].KeyID() != oldKey.KeyID() {
		t.Fatalf("retiring = %v, want the old key returned for revocation", retiring)
	}
	previous := retiring[0]
	active, err := LoadActiveServiceAccountKey(testServiceAccount)
	if err != nil || active.KeyID() != newKey.KeyID() {
		t.Fatalf("active = %v, %v; want the new key", active, err)
	}
	// The retired key is still on disk and can still sign its own revoke.
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{oldKey.KeyID(): KeyRetired, newKey.KeyID(): KeyActive})
	if _, err := previous.signAssertion(time.Now(), testServiceAccount, "https://x/revoke", nil); err != nil {
		t.Fatalf("retired key cannot sign its revoke: %v", err)
	}
	if _, err := ActivateServiceAccountKey(ctx, previous); err == nil {
		t.Fatal("a retired key was re-activated")
	}
	// Re-activating the active key resumes the pending retirement.
	again, err := ActivateServiceAccountKey(ctx, active)
	if err != nil || len(again) != 1 || again[0].KeyID() != oldKey.KeyID() {
		t.Fatalf("re-activate: retiring %v err %v; want the old key still owed a revoke", again, err)
	}

	if err := DeleteServiceAccountKey(ctx, active); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActiveServiceAccountKey(testServiceAccount); err == nil || !strings.Contains(err.Error(), "entire auth service-account enroll") {
		t.Fatalf("after deleting the active key: err = %v, want the enroll hint", err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{oldKey.KeyID(): KeyRetired})
	if err := DeleteServiceAccountKey(ctx, previous); err != nil {
		t.Fatal(err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{})
}

func TestReadKeyFile_WarnsOnLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permission bits")
	}
	configDir := isolateServiceAccountDirs(t)
	var warn bytes.Buffer
	prev := keyPermsWarnW
	keyPermsWarnW = &warn
	t.Cleanup(func() { keyPermsWarnW = prev })

	key := newTestKey(t, "https://us.auth.example.com")
	if _, err := CreateServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(configDir, ServiceAccountKeyFileName(key.KeyID())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActiveServiceAccountKey(testServiceAccount); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "accessible by group/others") {
		t.Fatalf("warning = %q, want a loose-permission warning", warn.String())
	}
}

func TestServiceAccountKey_ZeroValueErrorsInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	var k ServiceAccountKey
	if _, err := k.PublicKeyPEM(); err == nil {
		t.Error("PublicKeyPEM on a zero key succeeded")
	}
	if _, err := k.SignAssertion(time.Now()); err == nil {
		t.Error("SignAssertion on a zero key succeeded")
	}
	if _, err := k.signAssertion(time.Now(), "x", "y", nil); err == nil {
		t.Error("signAssertion on a zero key succeeded")
	}
}

// A key created for a plain-HTTP dev server under --insecure-http-auth must
// stay listable and deletable without the flag; only network use needs it.
func TestServiceAccountKey_InsecureServerUsableOnlyWithOptIn(t *testing.T) {
	isolateServiceAccountDirs(t)
	insecureHTTPOverride.Store(true)
	t.Cleanup(func() { insecureHTTPOverride.Store(false) })
	key := newTestKey(t, "http://dev-core.internal:8080")
	if _, err := CreateServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	insecureHTTPOverride.Store(false)

	loaded, err := LoadActiveServiceAccountKey(testServiceAccount)
	if err != nil {
		t.Fatalf("loading a dev-server key without the flag: %v", err)
	}
	if _, err := mintServiceAccountSession(context.Background(), loaded, time.Now()); err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("exchange without the flag: err = %v, want a plain-HTTP refusal", err)
	}
	if err := RevokeServiceAccountKey(context.Background(), loaded); err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("revoke without the flag: err = %v, want a plain-HTTP refusal", err)
	}
}

func TestGenerateServiceAccountKey_RejectsInsecureOrNonOriginServer(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"http://auth.example.com", "https://auth.example.com/path", "https://user@auth.example.com", "auth.example.com", "ftp://auth.example.com"} {
		if _, err := GenerateServiceAccountKey(testServiceAccount, server, "", time.Now()); err == nil {
			t.Errorf("server %q accepted, want error", server)
		}
	}
}

func TestSignAssertion_VerifiesAndCarriesClaims(t *testing.T) {
	t.Parallel()
	key, err := GenerateServiceAccountKey(testServiceAccount, "https://us.auth.example.com", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	assertion, err := key.SignAssertion(now)
	if err != nil {
		t.Fatal(err)
	}
	header, claims := verifyTestAssertion(t, key, assertion)
	if header["alg"] != "EdDSA" || header["kid"] != key.KeyID() {
		t.Fatalf("header = %v", header)
	}
	if claims["iss"] != testServiceAccount || claims["sub"] != testServiceAccount {
		t.Fatalf("iss/sub = %v/%v, want %s", claims["iss"], claims["sub"], testServiceAccount)
	}
	if claims["aud"] != "https://us.auth.example.com/api/authz/sts" {
		t.Fatalf("aud = %v", claims["aud"])
	}
	if exp, _ := claims["exp"].(float64); int64(exp) != now.Add(assertionTTL).Unix() { //nolint:errcheck // zero fails the comparison
		t.Fatalf("exp = %v, want iat+%s", claims["exp"], assertionTTL)
	}
	again, err := key.SignAssertion(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, c2 := verifyTestAssertion(t, key, again); c2["jti"] == claims["jti"] {
		t.Fatal("two assertions share a jti")
	}
}

// verifyTestAssertion checks the signature the way the server would: against
// the registered PKIX public key, not the in-memory private key.
func verifyTestAssertion(t *testing.T, key *ServiceAccountKey, assertion string) (header, claims map[string]any) {
	t.Helper()
	pubPEM, err := key.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	return verifyTestAssertionPEM(t, pubPEM, assertion)
}

func verifyTestAssertionPEM(t *testing.T, pubPEM, assertion string) (header, claims map[string]any) {
	t.Helper()
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		t.Fatal("no PEM public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("public key type %T", parsed)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("assertion signature does not verify against the registered public key")
	}
	return decodeTestSegment(t, parts[0]), decodeTestSegment(t, parts[1])
}

func decodeTestSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// testSessionJWT is a JWT shaped like an sa-session token minted by keyID.
// The CLI reads only its claims, so the signature is junk.
func testSessionJWT(sub, aud, keyID string, exp time.Time) string {
	return testSessionJWTClaims(fmt.Sprintf(`{"sub":%q,"aud":%q,"sa_key_id":%q,"exp":%d}`, sub, aud, keyID, exp.Unix()))
}

func testSessionJWTClaims(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"EdDSA","typ":"JWT"}`)) + "." + enc([]byte(claims)) + "." + enc([]byte("sig"))
}

// fakeSTS serves /oauth/token, verifying the request the way the proposed
// server branch would, and counts exchanges. session overrides the minted
// token (for the claim-validation tests).
type fakeSTS struct {
	t       *testing.T
	srv     *httptest.Server
	key     *ServiceAccountKey
	calls   atomic.Int32
	reject  bool
	session func(key *ServiceAccountKey) string
}

func newFakeSTS(t *testing.T) *fakeSTS {
	t.Helper()
	f := &fakeSTS{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSTS) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/oauth/token" {
		http.NotFound(w, r)
		return
	}
	f.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	if f.reject {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
		return
	}
	if id, _, ok := r.BasicAuth(); !ok || id != OAuthClientID {
		f.t.Errorf("client basic auth = %q, %v; want %s", id, ok, OAuthClientID)
	}
	want := map[string]string{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token_type":   "urn:ietf:params:oauth:token-type:jwt",
		"requested_token_type": saSessionTokenType,
		"resource":             testServiceAccount,
		"audience":             f.key.Server(),
	}
	for field, v := range want {
		if got := r.PostForm.Get(field); got != v {
			f.t.Errorf("%s = %q, want %q", field, got, v)
		}
	}
	_, claims := verifyTestAssertion(f.t, f.key, r.PostForm.Get("subject_token"))
	if claims["aud"] != ServiceAccountAssertionAudience(f.key.Server()) {
		f.t.Errorf("assertion aud = %v", claims["aud"])
	}
	token := testSessionJWT(testServiceAccount, f.key.Server(), f.key.KeyID(), nowFunc().Add(time.Hour))
	if f.session != nil {
		token = f.session(f.key)
	}
	fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, token)
}

func setupServiceAccountWithSTS(t *testing.T) (*fakeSTS, *ServiceAccountKey) {
	t.Helper()
	isolateServiceAccountDirs(t)
	f := newFakeSTS(t)
	f.key = newTestKey(t, f.srv.URL)
	if _, err := CreateServiceAccountKey(context.Background(), f.key); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ServiceAccountEnvVar, testServiceAccount)
	return f, f.key
}

func pinClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now()
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = time.Now })
	return &now
}

// envServiceAccountToken mints through ServiceAccountTarget, the path
// LookupEnvToken takes, minus its https-only check (the fakes are loopback
// HTTP servers).
func envServiceAccountToken(t *testing.T) (string, bool, error) {
	t.Helper()
	target, ok, err := ServiceAccountTarget()
	if !ok || err != nil {
		return "", ok, err
	}
	token, err := target.TokenSource(context.Background())
	return token, true, err
}

func TestLookupEnvToken_ServiceAccountExchangesThenCaches(t *testing.T) {
	now := pinClock(t)
	sts, key := setupServiceAccountWithSTS(t)

	raw, ok, err := envServiceAccountToken(t)
	if err != nil || !ok {
		t.Fatalf("LookupEnvToken = ok %v, err %v", ok, err)
	}
	if claims := decodeTestSegment(t, strings.Split(raw, ".")[1]); claims["aud"] != key.Server() {
		t.Fatalf("minted token aud = %v, want %s", claims["aud"], key.Server())
	}
	if _, _, err := envServiceAccountToken(t); err != nil {
		t.Fatal(err)
	}
	if got := sts.calls.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1 (second lookup must use the cache)", got)
	}

	// Inside the refresh skew the cached session is no longer handed out.
	*now = now.Add(time.Hour - saSessionRefreshSkew + time.Second)
	if _, _, err := envServiceAccountToken(t); err != nil {
		t.Fatal(err)
	}
	if got := sts.calls.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2 after the cached session neared expiry", got)
	}
}

// Concurrent processes on one key share one exchange through the cache lock.
func TestServiceAccountToken_ConcurrentCallersShareOneExchange(t *testing.T) {
	sts, key := setupServiceAccountWithSTS(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := ServiceAccountToken(context.Background(), key); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := sts.calls.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1", got)
	}
}

func TestServiceAccountToken_RejectsUnusableSessions(t *testing.T) {
	exp := func() int64 { return time.Now().Add(time.Hour).Unix() }
	for name, session := range map[string]func(k *ServiceAccountKey) string{
		"another subject": func(k *ServiceAccountKey) string {
			return testSessionJWT("svc_01HKZ3J4N5P6Q7R8S9T0V1W2X4", k.Server(), k.KeyID(), time.Now().Add(time.Hour))
		},
		"another audience": func(k *ServiceAccountKey) string {
			return testSessionJWT(testServiceAccount, "https://evil.example.com", k.KeyID(), time.Now().Add(time.Hour))
		},
		// Routing takes the first URL audience, so a second origin next to
		// the trusted one would carry the bearer there.
		"a second origin before the server": func(k *ServiceAccountKey) string {
			return testSessionJWTClaims(fmt.Sprintf(`{"sub":%q,"aud":["https://evil.example.com",%q],"sa_key_id":%q,"exp":%d}`, testServiceAccount, k.Server(), k.KeyID(), exp()))
		},
		"a second origin after the server": func(k *ServiceAccountKey) string {
			return testSessionJWTClaims(fmt.Sprintf(`{"sub":%q,"aud":[%q,"https://evil.example.com"],"sa_key_id":%q,"exp":%d}`, testServiceAccount, k.Server(), k.KeyID(), exp()))
		},
		"no key lineage": func(k *ServiceAccountKey) string {
			return testSessionJWTClaims(fmt.Sprintf(`{"sub":%q,"aud":%q,"exp":%d}`, testServiceAccount, k.Server(), exp()))
		},
		"another key's lineage": func(k *ServiceAccountKey) string {
			return testSessionJWT(testServiceAccount, k.Server(), strings.Repeat("B", 43), time.Now().Add(time.Hour))
		},
		"already expired": func(k *ServiceAccountKey) string {
			return testSessionJWT(testServiceAccount, k.Server(), k.KeyID(), time.Now().Add(-time.Minute))
		},
		"far future": func(k *ServiceAccountKey) string {
			return testSessionJWT(testServiceAccount, k.Server(), k.KeyID(), time.Now().Add(30*24*time.Hour))
		},
		"not a JWT": func(*ServiceAccountKey) string { return "opaque" },
	} {
		t.Run(name, func(t *testing.T) {
			sts, key := setupServiceAccountWithSTS(t)
			sts.session = session
			if _, err := ServiceAccountToken(context.Background(), key); err == nil || !strings.Contains(err.Error(), "unusable session token") {
				t.Fatalf("err = %v, want the session to be refused", err)
			}
			if _, ok := readServiceAccountSession(key, time.Now()); ok {
				t.Fatal("a refused session was cached")
			}
		})
	}
}

// A cache entry is bound to server, account and key id: a file edited to
// claim another server is a miss, not a token sent there.
func TestServiceAccountToken_CacheBoundToServerAndAccount(t *testing.T) {
	sts, key := setupServiceAccountWithSTS(t)
	if _, err := ServiceAccountToken(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "entire")
	path := filepath.Join(cacheDir, serviceAccountSessionName(key.KeyID()))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache (looked in %s): %v", cacheDir, err)
	}
	var s serviceAccountSession
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	s.Server = "https://other.example.com"
	tampered, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServiceAccountToken(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := sts.calls.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2: a cache entry for another server must not be used", got)
	}
}

func TestLookupEnvToken_ServiceAccountRejectedFailsClosed(t *testing.T) {
	sts, _ := setupServiceAccountWithSTS(t)
	sts.reject = true
	_, ok, err := envServiceAccountToken(t)
	if !ok {
		t.Fatal("ok = false; a configured service account must commit the caller to the env path")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), ServiceAccountEnvVar) {
		t.Fatalf("err = %v, want an invalid_grant error naming %s", err, ServiceAccountEnvVar)
	}
}

func TestLookupEnvToken_Precedence(t *testing.T) {
	sts, _ := setupServiceAccountWithSTS(t)

	t.Setenv(EnvTokenVar, "explicit-token")
	raw, ok, err := LookupEnvToken(context.Background())
	if err != nil || !ok || raw != "explicit-token" {
		t.Fatalf("with ENTIRE_TOKEN set: raw %q ok %v err %v; want ENTIRE_TOKEN verbatim", raw, ok, err)
	}
	if _, ok, _ := ServiceAccountTarget(); ok { //nolint:errcheck // only ok matters
		t.Fatal("ServiceAccountTarget ok while ENTIRE_TOKEN is set")
	}
	if EnvCredential() != CredentialFromEnvToken {
		t.Fatalf("EnvCredential = %q, want env_token", EnvCredential())
	}
	if sts.calls.Load() != 0 {
		t.Fatal("service account exchanged although ENTIRE_TOKEN wins")
	}

	os.Unsetenv(EnvTokenVar)
	if EnvCredential() != CredentialFromServiceAccount {
		t.Fatalf("EnvCredential = %q, want service_account", EnvCredential())
	}
	t.Setenv(ServiceAccountEnvVar, " ")
	if _, ok, err := LookupEnvToken(context.Background()); !ok || err == nil {
		t.Fatalf("blank %s: ok %v err %v; want a fail-closed error", ServiceAccountEnvVar, ok, err)
	}

	os.Unsetenv(ServiceAccountEnvVar)
	if _, ok, err := LookupEnvToken(context.Background()); ok || err != nil {
		t.Fatalf("nothing configured: ok %v err %v; want ok=false", ok, err)
	}
}

// A configured account with no key on disk fails closed: no exchange, no
// fall-back to a stored login.
func TestLookupEnvToken_ServiceAccountWithoutKeyFailsClosed(t *testing.T) {
	isolateServiceAccountDirs(t)
	t.Setenv(ServiceAccountEnvVar, testServiceAccount)
	_, ok, err := LookupEnvToken(context.Background())
	if !ok || err == nil || !strings.Contains(err.Error(), "no active key") {
		t.Fatalf("ok %v err %v; want a fail-closed missing-key error", ok, err)
	}
}

func TestServiceAccountToken_CacheIgnoredAfterKeyRotation(t *testing.T) {
	sts, key := setupServiceAccountWithSTS(t)
	if _, err := ServiceAccountToken(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	rotated := newTestKey(t, key.Server())
	sts.key = rotated
	if _, err := ServiceAccountToken(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}
	if got := sts.calls.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2: a session minted by the old key must not be reused", got)
	}
}

// The STS exchange carries a bearer assertion: a redirect must not forward it
// to another origin. auth-go's sts.Client enforces this; the test pins it.
func TestMintServiceAccountSession_RefusesRedirect(t *testing.T) {
	isolateServiceAccountDirs(t)
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	t.Cleanup(elsewhere.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	key := newTestKey(t, redirector.URL)
	if _, err := mintServiceAccountSession(context.Background(), key, time.Now()); err == nil {
		t.Fatal("a redirected exchange succeeded")
	}
	if leaked.Load() != 0 {
		t.Fatal("the assertion was forwarded to the redirect target")
	}
}

// Notices treat an exported-but-empty variable as unset, as they always have.
func TestEnvCredentialNonEmpty(t *testing.T) {
	isolateServiceAccountDirs(t)
	t.Setenv(EnvTokenVar, "")
	t.Setenv(ServiceAccountEnvVar, "")
	if EnvCredentialNonEmpty() {
		t.Fatal("empty variables reported as a configured credential")
	}
	if !EnvCredentialConfigured() {
		t.Fatal("set-but-empty variables must still commit resolution to the env path")
	}
	t.Setenv(ServiceAccountEnvVar, testServiceAccount)
	if !EnvCredentialNonEmpty() {
		t.Fatal("a service account was not reported")
	}
}

// A key whose server is not https cannot feed the ENTIRE_TOKEN-compatible
// paths (their aud check refuses plain HTTP); LookupEnvToken says so before
// any exchange instead of failing later on the aud.
func TestLookupEnvToken_RefusesPlainHTTPKeyServer(t *testing.T) {
	sts, _ := setupServiceAccountWithSTS(t) // a loopback http server
	_, ok, err := LookupEnvToken(context.Background())
	if !ok || err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("ok %v err %v; want a fail-closed https error", ok, err)
	}
	if sts.calls.Load() != 0 {
		t.Fatal("exchanged although the session could not be used")
	}
}
