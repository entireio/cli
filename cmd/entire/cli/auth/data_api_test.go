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

// The success path: discovery picks a context and its refreshed login JWT is
// the bearer — no RFC 8693 exchange. The core would be asked to exchange if we
// still did; make any request to it fail the test.
func TestResolveDataAPIToken_ReturnsLoginJWTWithoutExchange(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected core request %s %s: the login JWT is the bearer, nothing to exchange", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	svc := tokenstore.CoreKeyringService(srv.URL)
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"me","exp":%d}`, srv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: srv.URL, Handle: "me", KeychainService: svc}

	stubResolveContextForAPI(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})

	token, err := ResolveDataAPIToken(context.Background(), "https://data.example")
	if err != nil {
		t.Fatalf("ResolveDataAPIToken: %v", err)
	}
	if token != jwt {
		t.Fatal("token must be the stored login JWT, verbatim")
	}
}

func TestResolveDataAPIToken_UsesPlainHTTPDiscoveryForLoopbackDataOrigin(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	coreSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected core request %s %s", r.Method, r.URL.Path)
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
	if token != jwt {
		t.Fatal("token must be the stored login JWT, verbatim")
	}
}

// A context that exists and has a keychain slot but no stored token must
// surface an error unwrapping to ErrNotLoggedIn, so every ResolveDataAPIToken
// caller (activity, search, recap, dispatch) renders its `entire login`
// guidance instead of a raw failure. Re-pins the coverage that lived on the
// deleted exchange provider.
func TestResolveDataAPIToken_NotLoggedInPreservesSentinel(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	ctxObj := &contexts.Context{Name: "me@core", CoreURL: "https://core.example", Handle: "me", KeychainService: "kc:me"}
	stubResolveContextForAPI(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})

	_, err := ResolveDataAPIToken(context.Background(), "https://data.example")
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("error must unwrap to ErrNotLoggedIn, got %v", err)
	}
}
