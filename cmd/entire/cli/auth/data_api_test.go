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

// The default path: no ENTIRE_API_BASE_URL, so the selected login decides both
// host and bearer. A staging login talks to partial.to, never to entire.io.
func TestResolveDataAPI_FollowsSelectedLogin(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	_, stagingJWT := seedProdAndStagingContexts(t, configDir, stagingFixture.name)

	got, err := ResolveDataAPI(context.Background())
	if err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if got.BaseURL != "https://partial.to" {
		t.Fatalf("BaseURL = %q, want https://partial.to", got.BaseURL)
	}
	if got.Token != stagingJWT {
		t.Fatal("token must be the staging login JWT, verbatim")
	}
}

// A local-dev login server is not the web app (that runs on its own port), so
// a loopback login has no site to derive; the error names the override.
func TestResolveDataAPI_LoopbackLoginNamesOverride(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	writeActiveContext(t, configDir, "dev", "http://localhost:8787", "me", "kc:dev")

	_, err := ResolveDataAPI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ENTIRE_API_BASE_URL") {
		t.Fatalf("err = %v, want it to name ENTIRE_API_BASE_URL", err)
	}
}

// ENTIRE_TOKEN outranks every saved login, as it does for the control plane:
// the token is the bearer verbatim and its aud's site is the host. Nothing is
// announced — no saved login is acting.
func TestResolveDataAPI_FollowsEnvToken(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	seedProdAndStagingContexts(t, configDir, prodFixture.name)
	envJWT := makeJWT(t, fmt.Sprintf(`{"aud":%q,"exp":%d}`, stagingCoreURL, time.Now().Add(time.Hour).Unix()))
	t.Setenv(EnvTokenVar, envJWT)
	var notice strings.Builder
	CaptureContextNoticeForTest(t, &notice)

	got, err := ResolveDataAPI(context.Background())
	if err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if got.BaseURL != "https://partial.to" || got.Token != envJWT {
		t.Fatalf("got %+v, want the env token verbatim against https://partial.to", got)
	}
	if notice.Len() != 0 {
		t.Fatalf("notice = %q, want none under ENTIRE_TOKEN", notice.String())
	}
}

// Under an override the env token is still sent verbatim to the named host;
// discovery is for picking a saved login, and there is none to pick.
func TestResolveDataAPI_EnvTokenUnderOverrideSkipsDiscovery(t *testing.T) {
	isolateCellClientEnv(t, "https://data.example")
	envJWT := makeJWT(t, fmt.Sprintf(`{"aud":%q,"exp":%d}`, prodCoreURL, time.Now().Add(time.Hour).Unix()))
	t.Setenv(EnvTokenVar, envJWT)
	stubResolveContextForAPI(t, func(_ context.Context, _, _, host string, _ *http.Client, _ clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		t.Errorf("discovery against %q must not run under ENTIRE_TOKEN", host)
		return nil, errors.New("unexpected discovery")
	})

	got, err := ResolveDataAPI(context.Background())
	if err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if got.BaseURL != "https://data.example" || got.Token != envJWT {
		t.Fatalf("got %+v, want the env token verbatim against the override", got)
	}
}

// A blank ENTIRE_TOKEN is a misconfigured runner, not a request to use the
// saved login: fail closed, as coreapi.New does.
func TestResolveDataAPI_BlankEnvTokenFailsClosed(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	seedProdAndStagingContexts(t, configDir, prodFixture.name)
	t.Setenv(EnvTokenVar, "")

	_, err := ResolveDataAPI(context.Background())
	if err == nil || !strings.Contains(err.Error(), EnvTokenVar) {
		t.Fatalf("err = %v, want a fail-closed error naming %s", err, EnvTokenVar)
	}
}

// A login server outside entire.io / partial.to / loopback has no known site;
// the error names the override that would supply one.
func TestResolveDataAPI_UnknownLoginServerNamesOverride(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	writeActiveContext(t, configDir, "acme", "https://auth.acme.com", "me", "kc:acme")

	_, err := ResolveDataAPI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ENTIRE_API_BASE_URL") {
		t.Fatalf("err = %v, want it to name ENTIRE_API_BASE_URL", err)
	}
}

func TestResolveDataAPI_NoLoginIsNotLoggedIn(t *testing.T) {
	isolateCellClientEnv(t, "")

	_, err := ResolveDataAPI(context.Background())
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("error must unwrap to ErrNotLoggedIn, got %v", err)
	}
	if _, err := DataBaseURL(); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("DataBaseURL error must unwrap to ErrNotLoggedIn, got %v", err)
	}
}

// ENTIRE_API_BASE_URL keeps host discovery: the named host says which saved
// login it trusts.
func TestResolveDataAPI_OverrideDiscoversAgainstHost(t *testing.T) {
	configDir := isolateCellClientEnv(t, "https://data.example")
	prodJWT, _ := seedProdAndStagingContexts(t, configDir, stagingFixture.name)
	stubResolveContextForAPI(t, func(_ context.Context, _, _, host string, _ *http.Client, _ clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		if host != "data.example" {
			return nil, fmt.Errorf("host = %q, want data.example", host)
		}
		return &contexts.Context{Name: prodFixture.name, CoreURL: prodCoreURL, Handle: "me", KeychainService: tokenstore.CoreKeyringService(prodCoreURL)}, nil
	})

	got, err := ResolveDataAPI(context.Background())
	if err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if got.BaseURL != "https://data.example" || got.Token != prodJWT {
		t.Fatalf("got %+v, want the override host with the login it trusts", got)
	}
}

// With several logins saved the acting one is named once, on stderr; with one
// there is nothing to say.
func TestResolveDataAPI_AnnouncesContextAmongSeveral(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	seedProdAndStagingContexts(t, configDir, stagingFixture.name)
	var notice strings.Builder
	CaptureContextNoticeForTest(t, &notice)

	for range 2 {
		if _, err := ResolveDataAPI(context.Background()); err != nil {
			t.Fatalf("ResolveDataAPI: %v", err)
		}
	}
	if got := notice.String(); got != "Using context 'me@partial'.\n" {
		t.Fatalf("notice = %q, want one 'Using context' line", got)
	}
}

// A login picked by host discovery is announced like a selected one.
func TestResolveDataAPI_OverrideAnnouncesContext(t *testing.T) {
	configDir := isolateCellClientEnv(t, "https://data.example")
	seedProdAndStagingContexts(t, configDir, stagingFixture.name)
	stubResolveContextForAPI(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return &contexts.Context{Name: prodFixture.name, CoreURL: prodCoreURL, Handle: "me", KeychainService: tokenstore.CoreKeyringService(prodCoreURL)}, nil
	})
	var notice strings.Builder
	CaptureContextNoticeForTest(t, &notice)

	if _, err := ResolveDataAPI(context.Background()); err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if got := notice.String(); got != "Using context 'me@entire'.\n" {
		t.Fatalf("notice = %q, want the discovered login named", got)
	}
}

// ENTIRE_TOKEN runs still get a site for printed links: the token's core.
func TestDataBaseURL_FollowsEnvToken(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "")
	t.Setenv(EnvTokenVar, makeJWT(t, fmt.Sprintf(`{"aud":%q,"exp":%d}`, stagingCoreURL, time.Now().Add(time.Hour).Unix())))

	got, err := DataBaseURL()
	if err != nil {
		t.Fatalf("DataBaseURL: %v", err)
	}
	if got != "https://partial.to" {
		t.Fatalf("DataBaseURL = %q, want https://partial.to", got)
	}
}

func TestResolveDataAPI_SingleLoginIsSilent(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	svc := tokenstore.CoreKeyringService(prodCoreURL)
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"me","exp":%d}`, prodCoreURL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	writeActiveContext(t, configDir, "me@entire", prodCoreURL, "me", svc)
	var notice strings.Builder
	CaptureContextNoticeForTest(t, &notice)

	if _, err := ResolveDataAPI(context.Background()); err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if notice.Len() != 0 {
		t.Fatalf("notice = %q, want none with a single login", notice.String())
	}
}
