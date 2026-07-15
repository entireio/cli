package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/auth-go/sts"

	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// These tests drive process-global state (the token-store backend, the
// discovery seam, the provider singleton) so they cannot run in parallel.

// stubResolveContextForAPI swaps the discovery seam for the duration of the
// test, restoring it after.
func stubResolveContextForAPI(t *testing.T, fn resolveContextFunc) {
	t.Helper()
	prev := resolveContextForAPI
	resolveContextForAPI = fn
	t.Cleanup(func() { resolveContextForAPI = prev })
}

// An API host that doesn't advertise discovery is an error naming the host —
// without /.well-known/entire-api.json we can't know which login servers it
// trusts, and there is no static fallback to guess with.
func TestResolveDataAPIToken_ErrsWhenDiscoveryUnavailable(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	stubResolveContextForAPI(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return nil, fmt.Errorf("%w: 404", clusterdiscovery.ErrDiscoveryUnavailable)
	})

	_, err := ResolveDataAPIToken(context.Background(), "https://entire.io")
	if !errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		t.Fatalf("want the discovery-unavailable error surfaced, got %v", err)
	}
	if !strings.Contains(err.Error(), "entire.io") {
		t.Fatalf("err = %q, want it to name the host", err)
	}
}

// A reachable API whose context selection fails is a real error the user must
// act on — it must surface, not silently fall back to static resolution.
func TestResolveDataAPIToken_SurfacesSelectionError(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	sentinel := errors.New("multiple login contexts can authenticate against API host entire.io")
	stubResolveContextForAPI(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return nil, sentinel
	})

	_, err := ResolveDataAPIToken(context.Background(), "https://entire.io")
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the selection error surfaced verbatim, got %v", err)
	}
}

// The success path: discovery picks a context, and the provider exchanges that
// context's login JWT at its core for an audience equal to the data host
// origin (the aud the API requires), returning the exchanged token. The
// audience is derived from the resource origin by the token manager, not read
// from discovery.
func TestResolveDataAPIToken_ExchangesForDataHostOrigin(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const dataOrigin = "https://data.example"
	const wantAudience = "https://data.example"

	var gotAudience, gotResource, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm() //nolint:errcheck // test handler
		gotGrant = r.FormValue("grant_type")
		gotAudience = r.FormValue("audience")
		gotResource = r.FormValue("resource")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"exchanged-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()

	// Seed a fresh login JWT for a context whose core is the STS server, so the
	// provider needs no refresh and goes straight to the exchange.
	svc := tokenstore.CoreKeyringService(srv.URL)
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"me","exp":%d}`, srv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: srv.URL, Handle: "me", KeychainService: svc}

	stubResolveContextForAPI(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})

	// allowInsecure flows from the loopback http core (srv.URL) automatically.
	token, err := ResolveDataAPIToken(context.Background(), dataOrigin)
	if err != nil {
		t.Fatalf("ResolveDataAPIToken: %v", err)
	}
	if token != "exchanged-token" {
		t.Fatalf("token = %q, want the exchanged token", token)
	}
	if gotGrant != sts.GrantTypeTokenExchange {
		t.Fatalf("grant_type = %q, want token-exchange", gotGrant)
	}
	if gotAudience != wantAudience {
		t.Fatalf("audience = %q, want the data host origin %q (derived from the resource)", gotAudience, wantAudience)
	}
	if want := mustOrigin(t, dataOrigin); gotResource != want {
		t.Fatalf("resource = %q, want the data origin %q", gotResource, want)
	}
}

func TestResolveDataAPIToken_UsesPlainHTTPDiscoveryForLoopbackDataOrigin(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var gotAudience string
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm() //nolint:errcheck // test handler
		gotAudience = r.FormValue("audience")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"loopback-exchanged-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer coreSrv.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"me","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}

	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != clusterdiscovery.APIPath {
			t.Errorf("discovery path = %q, want %q", r.URL.Path, clusterdiscovery.APIPath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"trusted_issuers":[%q]}`, coreSrv.URL)
	}))
	defer dataSrv.Close()
	dataHost := strings.TrimPrefix(dataSrv.URL, "http://")

	stubResolveContextForAPI(t, func(ctx context.Context, _ string, _ string, host string, c *http.Client, debugf clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		if host != dataHost {
			return nil, fmt.Errorf("host = %q, want %q", host, dataHost)
		}
		doc, err := clusterdiscovery.DiscoverAPI(ctx, host, c, debugf)
		if err != nil {
			return nil, err
		}
		if len(doc.TrustedIssuers) != 1 || doc.TrustedIssuers[0] != coreSrv.URL {
			return nil, fmt.Errorf("trusted issuers = %v, want %q", doc.TrustedIssuers, coreSrv.URL)
		}
		return ctxObj, nil
	})

	token, err := ResolveDataAPIToken(context.Background(), dataSrv.URL)
	if err != nil {
		t.Fatalf("ResolveDataAPIToken: %v", err)
	}
	if token != "loopback-exchanged-token" {
		t.Fatalf("token = %q, want the exchanged loopback token", token)
	}
	if gotAudience != mustOrigin(t, dataSrv.URL) {
		t.Fatalf("audience = %q, want loopback data origin %q", gotAudience, mustOrigin(t, dataSrv.URL))
	}
}

func TestNewRefreshingResourceProvider_Validation(t *testing.T) {
	t.Parallel()
	if _, err := NewRefreshingResourceProvider(nil, "https://data.example", nil, false); err == nil {
		t.Fatal("want error for nil context")
	}
	if _, err := NewRefreshingResourceProvider(&contexts.Context{Name: "x", CoreURL: "https://core.example"}, "https://data.example", nil, false); err == nil {
		t.Fatal("want error for a context with no keychain slot")
	}
}

// When the selected context has no stored token, the provider's error must
// still unwrap to ErrNotLoggedIn so callers (NewAuthenticatedAPIClient, search,
// dispatch) that branch on errors.Is render their login guidance — the
// regression the PR review flagged on the discovery path.
func TestNewRefreshingResourceProvider_NotLoggedInPreservesSentinel(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	c := &contexts.Context{Name: "me@core", CoreURL: "https://core.example", Handle: "me", KeychainService: "kc:me"}
	provider, err := NewRefreshingResourceProvider(c, "https://data.example", nil, false)
	if err != nil {
		t.Fatalf("NewRefreshingResourceProvider: %v", err)
	}
	_, err = provider(context.Background())
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("provider error must unwrap to ErrNotLoggedIn, got %v", err)
	}
}

func mustOrigin(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Scheme + "://" + u.Host
}
