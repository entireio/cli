package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// fakeLoginJWT builds an unsigned JWT whose payload carries the given
// home_jurisdiction claim — enough for the client-side routing decisions,
// which never verify the signature.
func fakeLoginJWT(t *testing.T, homeJurisdiction string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"home_jurisdiction": homeJurisdiction})
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func staticLogin(jwt string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return jwt, nil }
}

func TestExchangeCore(t *testing.T) {
	t.Parallel()
	const auCore = "https://au.auth.example.io"

	t.Run("same jurisdiction uses home core", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource(auCore, "https://au.example.io", auCore, "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "au"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want home core", core)
		}
	})

	t.Run("home core trailing slash is trimmed", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource(auCore+"/", "https://au.example.io", "", "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "au"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want trimmed home core", core)
		}
	})

	t.Run("home jurisdiction claim casing is folded", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource(auCore, "https://au.example.io", "", "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "AU"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want home core despite mixed-case claim", core)
		}
	})

	t.Run("cross jurisdiction uses advertised jurisdiction core", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource("https://eu.auth.example.io", "https://au.example.io", auCore+"/", "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "eu"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want advertised au core (trimmed)", core)
		}
	})

	t.Run("cross jurisdiction without advertised core is refused", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource("https://eu.auth.example.io", "https://au.example.io", "", "h", nil, nil)
		if _, err := s.exchangeCore(fakeLoginJWT(t, "eu")); err == nil {
			t.Fatal("expected error when no jurisdiction core is advertised")
		}
	})

	t.Run("non-https advertised core is refused", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource("https://eu.auth.example.io", "https://au.example.io", "http://au.auth.example.io", "h", nil, nil)
		if _, err := s.exchangeCore(fakeLoginJWT(t, "eu")); err == nil {
			t.Fatal("expected error for plaintext advertised core")
		}
	})

	t.Run("env path pins the token's core", func(t *testing.T) {
		t.Parallel()
		// sa-session JWTs carry no home_jurisdiction claim; the pinned core
		// must win without ever parsing the subject token for routing.
		s := newEnvJurisdictionTokenSource("https://core.us.entire.io/", "https://us.entire.io", "env-jwt", "", nil)
		core, err := s.exchangeCore("not-even-a-jwt")
		if err != nil {
			t.Fatal(err)
		}
		if core != "https://core.us.entire.io" {
			t.Fatalf("core = %q, want pinned trimmed core", core)
		}
	})
}

func TestJurisdictionToken_MintsPersistsAndReuses(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		if got := r.FormValue("scope"); got != "openid" {
			t.Errorf("scope = %q, want openid", got)
		}
		if got := r.FormValue("audience"); got != "https://eu.example.io" {
			t.Errorf("audience = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"juri-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	// Same-jurisdiction routing: home_jurisdiction "eu" matches the audience
	// label, so the exchange goes to homeCoreURL — the fake core.
	login := staticLogin(fakeLoginJWT(t, "eu"))
	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())

	token, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "juri-jwt" {
		t.Fatalf("token = %q", token)
	}
	if mints != 1 {
		t.Fatalf("mints = %d, want 1", mints)
	}

	// Same source: memoized, no second exchange.
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mints after memo hit = %d, want 1", mints)
	}

	// Fresh source (new process): served from the persisted keychain entry.
	s2 := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())
	token2, err := s2.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token2 != "juri-jwt" {
		t.Fatalf("token2 = %q", token2)
	}
	if mints != 1 {
		t.Fatalf("mints after keychain hit = %d, want 1", mints)
	}

	// A different handle must not share the token.
	s3 := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "other-account", login, core.Client())
	if _, err := s3.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Fatalf("mints for different handle = %d, want 2", mints)
	}
}

func TestJurisdictionToken_InvalidateDropsMemoAndKeychain(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"juri-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	login := staticLogin(fakeLoginJWT(t, "eu"))
	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mints = %d, want 1", mints)
	}

	s.Invalidate()

	// Memo gone: this source re-mints.
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Fatalf("mints after invalidate = %d, want 2", mints)
	}

	// Keychain entry gone too: a fresh source (next process) also re-mints
	// rather than reading a stale persisted copy. (The re-mint above stored
	// a new entry, so invalidate again to observe the delete.)
	s.Invalidate()
	s2 := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())
	if _, err := s2.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 3 {
		t.Fatalf("mints for fresh source after invalidate = %d, want 3", mints)
	}
}

func TestJurisdictionToken_EmptyPersistedTokenRemints(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	// A corrupted entry: valid future timestamp, empty token. Must not be
	// served as a bare "Bearer " header.
	service := jurisdictionKeyringService("https://eu.example.io")
	corrupted := tokenstore.TokenExpirationSeparator +
		strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	if err := tokenstore.Set(service, "toothbrush", corrupted); err != nil {
		t.Fatal(err)
	}

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", staticLogin(fakeLoginJWT(t, "eu")), core.Client())
	token, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh-jwt" || mints != 1 {
		t.Fatalf("token = %q mints = %d, want fresh mint over corrupted entry", token, mints)
	}
}

func TestJurisdictionToken_ExpiredPersistedTokenRemints(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	// Seed a token 1 minute from expiry — inside the 5m refresh buffer.
	service := jurisdictionKeyringService("https://eu.example.io")
	expiring := "stale-jwt" + tokenstore.TokenExpirationSeparator +
		strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	if err := tokenstore.Set(service, "toothbrush", expiring); err != nil {
		t.Fatal(err)
	}

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", staticLogin(fakeLoginJWT(t, "eu")), core.Client())
	token, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh-jwt" || mints != 1 {
		t.Fatalf("token = %q mints = %d, want fresh mint", token, mints)
	}
}

func TestEnvJurisdictionToken_MintsInMemoryOnly(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	const audience = "https://eu.example.io"
	const envToken = "env-sa-session-jwt"
	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		if got := r.FormValue("subject_token"); got != envToken {
			t.Errorf("subject_token = %q, want the ENTIRE_TOKEN verbatim", got)
		}
		if got := r.FormValue("audience"); got != audience {
			t.Errorf("audience = %q, want %q", got, audience)
		}
		if got := r.FormValue("scope"); got != "openid" {
			t.Errorf("scope = %q, want openid", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"env-juri-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	s := newEnvJurisdictionTokenSource(core.URL, audience, envToken, "", core.Client())
	token, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "env-juri-jwt" || mints != 1 {
		t.Fatalf("token = %q mints = %d, want one mint", token, mints)
	}

	// Same source: memoized, no second exchange.
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mints after memo hit = %d, want 1", mints)
	}

	// Invalidate (data-plane 401) drops the memo without touching the token
	// store — the next acquisition in this process re-mints.
	s.Invalidate()
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Fatalf("mints after invalidate = %d, want 2", mints)
	}

	// Nothing may be persisted: a fresh source (new helper process) re-mints,
	// and the token store has no entry under the jurisdiction service.
	s2 := newEnvJurisdictionTokenSource(core.URL, audience, envToken, "", core.Client())
	if _, err := s2.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 3 {
		t.Fatalf("mints for fresh env source = %d, want 3 (no persistence)", mints)
	}
	if _, err := tokenstore.Get(jurisdictionKeyringService(audience), ""); err == nil {
		t.Fatal("env path must not write the jurisdiction token to the token store")
	}
}

func TestEnvJurisdictionToken_ExchangeHintDecoratesFailure(t *testing.T) {
	t.Parallel()

	// A cross-jurisdiction ENTIRE_TOKEN reaches the wrong core, which
	// refuses the audience with an opaque invalid_target. The pre-computed
	// hint must ride the error so the CI operator learns the actual fix.
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_target"}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	const hint = "\nENTIRE_TOKEN was minted at X, but cluster Y's jurisdiction core is Z — point your CI auth url at Z"
	s := newEnvJurisdictionTokenSource(core.URL, "https://au.example.io", "env-jwt", hint, core.Client())
	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("expected the exchange to fail")
	}
	if !strings.HasSuffix(err.Error(), hint) {
		t.Fatalf("error must end with the cross-jurisdiction hint, got: %v", err)
	}
}
