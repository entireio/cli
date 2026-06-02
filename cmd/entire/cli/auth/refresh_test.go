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

func TestContextTokenStore_RoundTrip(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	st := contextTokenStore{service: "entire-core:https://core.example", handle: "alice"}

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
	if r, _ := tokenstore.Get(st.service+":refresh", st.handle); r != "" { //nolint:errcheck // read-back; only the value matters here
		t.Fatalf("refresh slot survived delete: %q", r)
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
	if err := tokenstore.Set(svc, "alice", tokenstore.EncodeTokenWithExpiration(expired, -3600)); err != nil {
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
	if err := tokenstore.Set(svc, "alice", tokenstore.EncodeTokenWithExpiration(expired, -3600)); err != nil {
		t.Fatalf("seed access token: %v", err)
	}
	if err := tokenstore.Set(svc+":refresh", "alice", "entr_old"); err != nil {
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
	if r, _ := tokenstore.Get(svc+":refresh", "alice"); r != "entr_new" { //nolint:errcheck // read-back
		t.Fatalf("rotated refresh token = %q, want entr_new", r)
	}
	enc, _ := tokenstore.Get(svc, "alice") //nolint:errcheck // read-back
	if access, _ := tokenstore.DecodeTokenWithExpiration(enc); access != newJWT {
		t.Fatalf("persisted access token not updated to the refreshed JWT")
	}
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
