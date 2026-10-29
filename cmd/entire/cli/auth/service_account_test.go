package auth

import (
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
	t.Setenv(EnvTokenVar, "")
	os.Unsetenv(EnvTokenVar)
	t.Setenv(ServiceAccountEnvVar, "")
	os.Unsetenv(ServiceAccountEnvVar)
	return configDir
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

func TestServiceAccountKey_SaveLoadRoundTrip(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)

	key, err := GenerateServiceAccountKey(testServiceAccount, "https://us.auth.example.com/", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if key.Server != "https://us.auth.example.com" {
		t.Fatalf("server = %q, want bare origin", key.Server)
	}
	name, err := SaveServiceAccountKey(key, false)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(configDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("key file perm = %o, want 600", perm)
		}
	}

	if _, err := SaveServiceAccountKey(key, false); !errors.Is(err, ErrServiceAccountKeyExists) {
		t.Fatalf("second save without overwrite: err = %v, want ErrServiceAccountKeyExists", err)
	}

	loaded, err := LoadServiceAccountKey(testServiceAccount)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.KeyID != key.KeyID || loaded.Server != key.Server {
		t.Fatalf("loaded key = %+v, want key id %q server %q", loaded, key.KeyID, key.Server)
	}
	wantPub, _ := key.PublicKeyPEM()   //nolint:errcheck // compared below
	gotPub, _ := loaded.PublicKeyPEM() //nolint:errcheck // compared below
	if gotPub != wantPub {
		t.Fatal("loaded key has a different public key")
	}
}

func TestLoadServiceAccountKey_MissingNamesCreateCommand(t *testing.T) {
	isolateServiceAccountDirs(t)
	_, err := LoadServiceAccountKey(testServiceAccount)
	if err == nil || !strings.Contains(err.Error(), "entire auth service-account key create") {
		t.Fatalf("err = %v, want the create hint", err)
	}
}

func TestGenerateServiceAccountKey_RejectsInsecureOrNonOriginServer(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"http://auth.example.com", "https://auth.example.com/path", "https://user@auth.example.com", "auth.example.com"} {
		if _, err := GenerateServiceAccountKey(testServiceAccount, server, time.Now()); err == nil {
			t.Errorf("server %q accepted, want error", server)
		}
	}
}

func TestSignAssertion_VerifiesAndCarriesClaims(t *testing.T) {
	t.Parallel()
	key, err := GenerateServiceAccountKey(testServiceAccount, "https://us.auth.example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	assertion, err := key.SignAssertion(now)
	if err != nil {
		t.Fatal(err)
	}
	header, claims := verifyTestAssertion(t, key, assertion)
	if header["alg"] != "EdDSA" || header["kid"] != key.KeyID {
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

// testSessionJWT is a JWT shaped like an sa-session token. The CLI only reads
// its aud (ParseEnvToken, which refuses alg:none), so the signature is junk.
func testSessionJWT(aud string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"EdDSA","typ":"JWT"}`)) + "." + enc([]byte(`{"sub":"`+testServiceAccount+`","aud":"`+aud+`"}`)) + "." + enc([]byte("sig"))
}

// newFakeSTS serves /oauth/token, verifying the request the way the proposed
// server branch would, and counts exchanges.
func newFakeSTS(t *testing.T, key **ServiceAccountKey, calls *atomic.Int32, reject bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if reject {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		if id, _, ok := r.BasicAuth(); !ok || id != OAuthClientID {
			t.Errorf("client basic auth = %q, %v; want %s", id, ok, OAuthClientID)
		}
		want := map[string]string{
			"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
			"subject_token_type":   "urn:ietf:params:oauth:token-type:jwt",
			"requested_token_type": saSessionTokenType,
			"resource":             testServiceAccount,
			"audience":             (*key).Server,
		}
		for field, v := range want {
			if got := r.PostForm.Get(field); got != v {
				t.Errorf("%s = %q, want %q", field, got, v)
			}
		}
		_, claims := verifyTestAssertion(t, *key, r.PostForm.Get("subject_token"))
		if claims["aud"] != ServiceAccountAssertionAudience((*key).Server) {
			t.Errorf("assertion aud = %v", claims["aud"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, testSessionJWT("https://us.auth.example.com"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupServiceAccountWithSTS(t *testing.T, reject bool) (*atomic.Int32, *ServiceAccountKey) {
	t.Helper()
	isolateServiceAccountDirs(t)
	var calls atomic.Int32
	var key *ServiceAccountKey
	srv := newFakeSTS(t, &key, &calls, reject)
	var err error
	key, err = GenerateServiceAccountKey(testServiceAccount, srv.URL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveServiceAccountKey(key, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ServiceAccountEnvVar, testServiceAccount)
	return &calls, key
}

func TestLookupEnvToken_ServiceAccountExchangesThenCaches(t *testing.T) {
	calls, _ := setupServiceAccountWithSTS(t, false)
	now := time.Now()
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = time.Now })

	raw, ok, err := LookupEnvToken(context.Background())
	if err != nil || !ok {
		t.Fatalf("LookupEnvToken = ok %v, err %v", ok, err)
	}
	if core, _, err := ParseEnvToken(raw); err != nil || core != "https://us.auth.example.com" {
		t.Fatalf("minted token does not pass ParseEnvToken: core %q, err %v", core, err)
	}
	if _, _, err := LookupEnvToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1 (second lookup must use the cache)", got)
	}

	// Inside the refresh skew the cached session is no longer handed out.
	now = now.Add(time.Hour - saSessionRefreshSkew + time.Second)
	if _, _, err := LookupEnvToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2 after the cached session neared expiry", got)
	}
}

func TestLookupEnvToken_ServiceAccountRejectedFailsClosed(t *testing.T) {
	setupServiceAccountWithSTS(t, true)
	_, ok, err := LookupEnvToken(context.Background())
	if !ok {
		t.Fatal("ok = false; a configured service account must commit the caller to the env path")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), ServiceAccountEnvVar) {
		t.Fatalf("err = %v, want an invalid_grant error naming %s", err, ServiceAccountEnvVar)
	}
}

func TestLookupEnvToken_Precedence(t *testing.T) {
	calls, _ := setupServiceAccountWithSTS(t, false)

	t.Setenv(EnvTokenVar, "explicit-token")
	raw, ok, err := LookupEnvToken(context.Background())
	if err != nil || !ok || raw != "explicit-token" {
		t.Fatalf("with ENTIRE_TOKEN set: raw %q ok %v err %v; want ENTIRE_TOKEN verbatim", raw, ok, err)
	}
	if _, ok, _ := ServiceAccountTarget(); ok { //nolint:errcheck // only ok matters
		t.Fatal("ServiceAccountTarget ok while ENTIRE_TOKEN is set")
	}
	if calls.Load() != 0 {
		t.Fatal("service account exchanged although ENTIRE_TOKEN wins")
	}

	os.Unsetenv(EnvTokenVar)
	t.Setenv(ServiceAccountEnvVar, " ")
	if _, ok, err := LookupEnvToken(context.Background()); !ok || err == nil {
		t.Fatalf("blank %s: ok %v err %v; want a fail-closed error", ServiceAccountEnvVar, ok, err)
	}

	os.Unsetenv(ServiceAccountEnvVar)
	if _, ok, err := LookupEnvToken(context.Background()); ok || err != nil {
		t.Fatalf("nothing configured: ok %v err %v; want ok=false", ok, err)
	}
}

func TestServiceAccountToken_CacheIgnoredAfterKeyRotation(t *testing.T) {
	calls, key := setupServiceAccountWithSTS(t, false)
	if _, err := ServiceAccountToken(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	rotated, err := GenerateServiceAccountKey(testServiceAccount, key.Server, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	*key = *rotated // the fake STS verifies against the current key
	if _, err := ServiceAccountToken(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2: a session minted by the old key must not be reused", got)
	}
}
