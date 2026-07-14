package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/auth-go/tokens"
	authtokenstore "github.com/entireio/auth-go/tokenstore"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// testCoreService is the keychain access-token service used across the
// contextTokenStore tests (paired refresh slot is RefreshService(it)).
const testCoreService = "entire-core:https://core.example"

func TestContextTokenStore_RoundTrip(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	st := contextTokenStore{service: testCoreService, handle: "alice"}

	// Missing → ErrNotFound.
	if _, err := st.LoadTokens(""); !errors.Is(err, authtokenstore.ErrNotFound) {
		t.Fatalf("LoadTokens on empty store: got %v, want ErrNotFound", err)
	}

	jwt := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	if err := st.SaveTokens("", tokens.TokenSet{
		AccessToken:  jwt,
		RefreshToken: "entr_refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}

	got, err := st.LoadTokens("")
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	if got.AccessToken != jwt {
		t.Fatalf("access token = %q, want the stored JWT", got.AccessToken)
	}
	if got.RefreshToken != "entr_refresh" {
		t.Fatalf("refresh token = %q, want %q", got.RefreshToken, "entr_refresh")
	}

	if err := st.DeleteTokens(""); err != nil {
		t.Fatalf("DeleteTokens: %v", err)
	}
	if _, err := st.LoadTokens(""); !errors.Is(err, authtokenstore.ErrNotFound) {
		t.Fatal("LoadTokens after delete: want ErrNotFound")
	}
	if r, _ := tokenstore.Get(tokenstore.RefreshService(st.service), st.handle); r != "" { //nolint:errcheck // read-back; only the value matters here
		t.Fatalf("refresh slot survived delete: %q", r)
	}
}

// A non-NotFound failure reading the refresh slot must surface, not be
// swallowed — swallowing would discard a valid refresh token and force a
// re-login on a transient keyring/file-store hiccup.
func TestContextTokenStore_LoadTokens_RefreshReadErrorSurfaces(t *testing.T) {
	svc := testCoreService

	// Seed a valid access token through a clean backend.
	path := filepath.Join(t.TempDir(), "tokens.json")
	seedRestore := tokenstore.UseFileBackendForTesting(path)
	access := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	if err := tokenstore.Set(svc, "alice", tokenstore.EncodeTokenWithExpiration(access, 3600)); err != nil {
		t.Fatalf("seed access: %v", err)
	}
	seedRestore()

	// Fail only the refresh-slot read; the access read still succeeds.
	failRefreshGet := func(service, _ string) bool { return service == tokenstore.RefreshService(svc) }
	restore := tokenstore.UseFailingGetBackendForTesting(path, failRefreshGet)
	t.Cleanup(restore)

	st := contextTokenStore{service: svc, handle: "alice"}
	if _, err := st.LoadTokens(""); err == nil {
		t.Fatal("LoadTokens: want error when the refresh-slot read fails, got nil")
	}
}

func TestNewRefreshingLoginProvider_Validation(t *testing.T) {
	if _, err := NewRefreshingLoginProvider(nil, nil, false); err == nil {
		t.Error("nil context: want error")
	}
	if _, err := NewRefreshingLoginProvider(&contexts.Context{Name: "x"}, nil, false); err == nil {
		t.Error("context without keychain slot: want error")
	}
}

// A still-valid login JWT is returned with no network call — proven by a
// transport that fails the test if invoked.
func TestNewRefreshingLoginProvider_FreshTokenNoNetwork(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	svc := tokenstore.CoreKeyringService("https://core.example")
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "alice", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	c := &contexts.Context{Name: "alice@core", CoreURL: "https://core.example", Handle: "alice", KeychainService: svc}
	provider, err := NewRefreshingLoginProvider(c, failRoundTripper(t), false)
	if err != nil {
		t.Fatalf("NewRefreshingLoginProvider: %v", err)
	}
	got, err := provider(context.Background())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != jwt {
		t.Fatalf("provider returned %q, want the stored valid JWT", got)
	}
}

// Expired token with no refresh token behaves like the old read-only path:
// a clear re-login error, not a crash.
func TestNewRefreshingLoginProvider_ExpiredNoRefresh(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	svc := tokenstore.CoreKeyringService("https://core.example")
	expired := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(-time.Hour).Unix()))
	if err := tokenstore.Set(svc, "alice", expired+tokenstore.TokenExpirationSeparator+"0"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	c := &contexts.Context{Name: "alice@core", CoreURL: "https://core.example", Handle: "alice", KeychainService: svc}
	provider, err := NewRefreshingLoginProvider(c, failRoundTripper(t), false)
	if err != nil {
		t.Fatalf("NewRefreshingLoginProvider: %v", err)
	}
	_, err = provider(context.Background())
	if err == nil {
		t.Fatal("expired token with no refresh: want a re-login error")
	}
	// The hint must name the core so a multi-core user re-logs into the right one.
	if got := err.Error(); !strings.Contains(got, "https://core.example") || !strings.Contains(got, "entire login") {
		t.Fatalf("re-login error = %q, want it to name the core and the login command", got)
	}
}

// The full path: an expired access token is silently re-minted from the
// stored refresh token, and the rotated refresh token is persisted.
func TestNewRefreshingLoginProvider_RefreshesAndRotates(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	newJWT := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(time.Hour).Unix()))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.FormValue("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", got)
		}
		if got := r.FormValue("refresh_token"); got != "entr_old" {
			t.Errorf("refresh_token = %q, want entr_old", got)
		}
		if r.FormValue("client_id") == "" {
			t.Error("missing client_id")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"access_token":%q,"refresh_token":"entr_new","token_type":"Bearer","expires_in":3600}`, newJWT)
	}))
	defer srv.Close()

	svc := tokenstore.CoreKeyringService(srv.URL)
	expired := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, srv.URL, time.Now().Add(-time.Hour).Unix()))
	if err := tokenstore.Set(svc, "alice", expired+tokenstore.TokenExpirationSeparator+"0"); err != nil {
		t.Fatalf("seed access token: %v", err)
	}
	if err := tokenstore.Set(tokenstore.RefreshService(svc), "alice", "entr_old"); err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}

	c := &contexts.Context{Name: "alice@core", CoreURL: srv.URL, Handle: "alice", KeychainService: svc}
	// allowInsecureHTTP: the httptest server is http://127.0.0.1.
	provider, err := NewRefreshingLoginProvider(c, srv.Client().Transport, true)
	if err != nil {
		t.Fatalf("NewRefreshingLoginProvider: %v", err)
	}

	got, err := provider(context.Background())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != newJWT {
		t.Fatalf("provider returned the old token, want the refreshed one")
	}

	// Rotated refresh token persisted, and the new access token cached.
	if r, _ := tokenstore.Get(tokenstore.RefreshService(svc), "alice"); r != "entr_new" { //nolint:errcheck // read-back
		t.Fatalf("rotated refresh token = %q, want entr_new", r)
	}
	enc, _ := tokenstore.Get(svc, "alice") //nolint:errcheck // read-back
	if access, _ := tokenstore.DecodeTokenWithExpiration(enc); access != newJWT {
		t.Fatalf("persisted access token not updated to the refreshed JWT")
	}
}

// SaveTokens must never leave a fresh access token paired with a stale
// refresh token: the server single-use-rotates, so that pairing looks healthy
// until the access token expires, then the dead refresh token forces a
// re-login. The store persists refresh-first to invert both failure modes.
func TestContextTokenStore_SaveTokens_RefreshFirstOrdering(t *testing.T) {
	t.Run("refresh write fails: access slot untouched", func(t *testing.T) {
		svc := testCoreService
		path := filepath.Join(t.TempDir(), "tokens.json")

		// Seed an existing good pair through a clean backend first — the fault
		// backend installed below would reject the refresh-slot seed too.
		oldAccess := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(time.Hour).Unix()))
		seedRestore := tokenstore.UseFileBackendForTesting(path)
		if err := tokenstore.Set(svc, "alice", tokenstore.EncodeTokenWithExpiration(oldAccess, 3600)); err != nil {
			t.Fatalf("seed access: %v", err)
		}
		if err := tokenstore.Set(tokenstore.RefreshService(svc), "alice", "entr_old"); err != nil {
			t.Fatalf("seed refresh: %v", err)
		}
		seedRestore()

		// Now point at the SAME file but fail any refresh-slot write.
		failRefresh := func(service, _ string) bool { return service == tokenstore.RefreshService(svc) }
		restore := tokenstore.UseFailingBackendForTesting(path, failRefresh)
		t.Cleanup(restore)

		st := contextTokenStore{service: svc, handle: "alice"}
		newAccess := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(2*time.Hour).Unix()))
		err := st.SaveTokens("", tokens.TokenSet{AccessToken: newAccess, RefreshToken: "entr_new", ExpiresAt: time.Now().Add(2 * time.Hour)})
		if err == nil {
			t.Fatal("SaveTokens: want error when refresh write fails")
		}
		// The access slot must NOT have advanced — aborting before the access
		// write preserves the old (still-mintable) pair.
		enc, _ := tokenstore.Get(svc, "alice") //nolint:errcheck // read-back
		if access, _ := tokenstore.DecodeTokenWithExpiration(enc); access != oldAccess {
			t.Fatalf("access slot advanced despite refresh-write failure: a fresh access token is now paired with a stale refresh token")
		}
	})

	t.Run("access write fails: refresh slot already advanced (self-heals)", func(t *testing.T) {
		svc := testCoreService
		failAccess := func(service, _ string) bool { return service == svc }
		restore := tokenstore.UseFailingBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"), failAccess)
		t.Cleanup(restore)

		st := contextTokenStore{service: svc, handle: "alice"}
		newAccess := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example","handle":"alice","exp":%d}`, time.Now().Add(2*time.Hour).Unix()))
		err := st.SaveTokens("", tokens.TokenSet{AccessToken: newAccess, RefreshToken: "entr_new", ExpiresAt: time.Now().Add(2 * time.Hour)})
		if err == nil {
			t.Fatal("SaveTokens: want error when access write fails")
		}
		// Refresh-first means the rotated refresh token is already persisted, so
		// the next load re-mints a fresh access token rather than replaying a
		// dead refresh token.
		if r, _ := tokenstore.Get(tokenstore.RefreshService(svc), "alice"); r != "entr_new" { //nolint:errcheck // read-back
			t.Fatalf("refresh slot = %q, want entr_new persisted before the access write", r)
		}
	})
}

func failRoundTripper(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected HTTP call to %s — a fresh token must not hit the network", r.URL)
		return nil, errors.New("unexpected network call")
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
