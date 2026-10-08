package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func TestHomeJurisdictionFromLoginJWT(t *testing.T) {
	t.Parallel()
	jwt := makeJWT(t, fmt.Sprintf(`{"home_jurisdiction":"us","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	got, err := HomeJurisdictionFromLoginJWT(jwt)
	if err != nil {
		t.Fatalf("HomeJurisdictionFromLoginJWT: %v", err)
	}
	if got != "us" {
		t.Fatalf("jurisdiction = %q, want us", got)
	}
}

func TestIsBFFOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		origin string
		want   bool
	}{
		{"https://entire.io", true},                     // prod BFF
		{"https://staging.entire.io", true},             // prod apex variant
		{"https://partial.to", true},                    // staging BFF
		{"https://us.partial.to", true},                 // staging apex variant
		{"https://aws-us-east-2.api.entire.io", false},  // direct cell
		{"https://aws-eu-west-1.api.partial.to", false}, // staging direct cell
		{"http://127.0.0.1:8099", false},                // local dev
		{"http://localhost:8787", false},                // local dev
	}
	for _, tc := range tests {
		if got := isBFFOrigin(tc.origin); got != tc.want {
			t.Errorf("isBFFOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestEntireSite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		core string
		want string
	}{
		{"https://us.auth.entire.io", "entire.io"},
		{"https://eu.auth.entire.io", "entire.io"},
		{"https://us.auth.partial.to", "partial.to"},
		{"http://127.0.0.1:9000", ""},
		{"https://auth.example.com", ""},
	}
	for _, tc := range tests {
		if got := EntireSite(tc.core); got != tc.want {
			t.Errorf("EntireSite(%q) = %q, want %q", tc.core, got, tc.want)
		}
	}
}

func TestRequireSafeExchangeURL(t *testing.T) {
	// Exercises the plaintext-downgrade guard. Reset the process-global insecure
	// override (no public setter) so the assertion is order-independent.
	prev := insecureHTTPOverride.Load()
	insecureHTTPOverride.Store(false)
	t.Cleanup(func() { insecureHTTPOverride.Store(prev) })

	tests := []struct {
		raw     string
		wantErr bool
	}{
		{"https://us.entire.io", false},
		{"https://aws-eu-west-1.api.entire.io", false},
		{"http://127.0.0.1:9000", false}, // loopback allowed
		{"http://localhost:8787", false}, // loopback allowed
		{"http://evil.example.com", true},
		{"ftp://evil.example.com", true},  // non-https, non-loopback
		{"ws://evil.example.com", true},   // scheme-relative-ish
		{"//evil.example.com/path", true}, // no scheme
		{"", true},                        // empty
		{"https://", true},                // no host
	}
	for _, tc := range tests {
		err := requireSafeExchangeURL("test", tc.raw)
		if tc.wantErr && err == nil {
			t.Errorf("requireSafeExchangeURL(%q) = nil, want error", tc.raw)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("requireSafeExchangeURL(%q) = %v, want nil", tc.raw, err)
		}
	}

	// With the insecure override on, a plain-http non-loopback host is allowed.
	insecureHTTPOverride.Store(true)
	if err := requireSafeExchangeURL("test", "http://dev.example.com"); err != nil {
		t.Errorf("with insecure override: got %v, want nil", err)
	}
}

func TestTargetJurisdictionRejectsBadLabel(t *testing.T) {
	t.Parallel()
	bad := makeJWT(t, fmt.Sprintf(`{"home_jurisdiction":"us.auth.evil.tld","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	if _, err := targetJurisdiction(nil, bad); err == nil {
		t.Fatal("expected rejection of non-label home_jurisdiction")
	}
	good := makeJWT(t, fmt.Sprintf(`{"home_jurisdiction":"us","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	if got, err := targetJurisdiction(nil, good); err != nil || got != "us" {
		t.Fatalf("targetJurisdiction(good) = %q, %v; want us, nil", got, err)
	}
	// An explicit target wins over the JWT claim.
	if got, err := targetJurisdiction(&CellTarget{Jurisdiction: "eu"}, good); err != nil || got != "eu" {
		t.Fatalf("targetJurisdiction(target=eu) = %q, %v; want eu, nil", got, err)
	}
	// An uppercase JWT claim is case-folded rather than rejected by the strict
	// lowercase label check.
	upper := makeJWT(t, fmt.Sprintf(`{"home_jurisdiction":"US","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	if got, err := targetJurisdiction(nil, upper); err != nil || got != "us" {
		t.Fatalf("targetJurisdiction(US) = %q, %v; want us, nil", got, err)
	}
}

func TestNewEntireAPICellClient_RoutesThroughHomeCell(t *testing.T) {
	isolateCellClientEnv(t, "https://entire.io")

	var gotReposHost, gotAuthorization string
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case clustersAPIPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test handler
				"clusters": []map[string]any{{
					"jurisdiction": "us",
					"isDefault":    true,
					"apiUrl":       "http://" + r.Host,
				}},
			})
		case "/api/v1/repos":
			gotReposHost = r.Host
			gotAuthorization = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"repos":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer coreSrv.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}

	cleanupDiscovery := SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})
	t.Cleanup(cleanupDiscovery)

	t.Cleanup(SetCellExchangeTransportForTest(t, coreSrv.Client().Transport))

	client, err := NewEntireAPICellClient(context.Background(), false, nil)
	if err != nil {
		t.Fatalf("NewEntireAPICellClient: %v", err)
	}
	resp, err := client.Get(context.Background(), "/api/v1/repos")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if gotReposHost == "" {
		t.Fatal("cell repos request was not received")
	}
	if gotAuthorization != "Bearer "+loginJWT {
		t.Fatalf("Authorization = %q, want login JWT bearer", gotAuthorization)
	}
}

func TestNewEntireAPICellClient_KeepsDirectCellBaseURL(t *testing.T) {
	const cellBase = "https://aws-us-east-2.api.entire.io"
	isolateCellClientEnv(t, cellBase)

	var exchangeHit, clustersHit bool
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case oauthTokenPath:
			exchangeHit = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"cell-identity-token","token_type":"Bearer","expires_in":3600}`)
		case clustersAPIPath:
			clustersHit = true
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer coreSrv.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}

	cleanupDiscovery := SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})
	t.Cleanup(cleanupDiscovery)

	t.Cleanup(SetCellExchangeTransportForTest(t, coreSrv.Client().Transport))

	client, err := NewEntireAPICellClient(context.Background(), false, nil)
	if err != nil {
		t.Fatalf("NewEntireAPICellClient: %v", err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	// A direct cell origin must not trigger cluster resolution or token exchange.
	if clustersHit {
		t.Error("direct cell URL should not resolve clusters")
	}
	if exchangeHit {
		t.Error("direct cell URL should not exchange the login token")
	}
	if !strings.HasSuffix(api.OriginOnly(cellBase), ".api.entire.io") {
		t.Fatalf("test precondition: %q is not a direct cell URL", cellBase)
	}
}

func TestResolveTargetCellBaseURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A direct cell origin (host with .api.) is kept verbatim, no resolution.
	if got, err := resolveTargetCellBaseURL(ctx, nil, "https://aws-us-east-2.api.entire.io", "us", "https://us.auth.entire.io", "login", nil); err != nil || got != "https://aws-us-east-2.api.entire.io" {
		t.Fatalf("direct cell: got %q, %v", got, err)
	}
	// A loopback (local dev) origin is kept verbatim.
	if got, err := resolveTargetCellBaseURL(ctx, nil, "http://127.0.0.1:8099", "us", "http://127.0.0.1:9000", "login", nil); err != nil || got != "http://127.0.0.1:8099" {
		t.Fatalf("loopback: got %q, %v", got, err)
	}
	// An explicit target wins over everything and is trimmed of a trailing slash.
	if got, err := resolveTargetCellBaseURL(ctx, &CellTarget{BaseURL: "https://eu.api.entire.io/"}, "https://entire.io", "eu", "https://eu.auth.entire.io", "login", nil); err != nil || got != "https://eu.api.entire.io" {
		t.Fatalf("target override: got %q, %v", got, err)
	}
	// A loopback origin with an explicitly pinned jurisdiction stays verbatim:
	// local dev serves a single cell with no jurisdiction catalog to consult.
	if got, err := resolveTargetCellBaseURL(ctx, &CellTarget{Jurisdiction: "us"}, "http://127.0.0.1:8099", "us", "http://127.0.0.1:9000", "login", nil); err != nil || got != "http://127.0.0.1:8099" {
		t.Fatalf("loopback + explicit jurisdiction: got %q, %v", got, err)
	}
	// A non-loopback DIRECT cell origin with an explicitly pinned jurisdiction
	// must NOT be dialed verbatim (it may name a different jurisdiction's cell):
	// resolve the pinned jurisdiction's own cell from the catalog instead.
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != clustersAPIPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"clusters":[{"jurisdiction":"eu","isDefault":true,"apiUrl":"https://eu.api.entire.io"}]}`)
	}))
	defer catalog.Close()
	if got, err := resolveTargetCellBaseURL(ctx, &CellTarget{Jurisdiction: "eu"}, "https://aws-us-east-2.api.entire.io", "eu", catalog.URL, "login", catalog.Client()); err != nil || got != "https://eu.api.entire.io" {
		t.Fatalf("direct cell + explicit jurisdiction: got %q, %v (want catalog-resolved eu cell)", got, err)
	}
	if got, err := resolveTargetCellBaseURL(ctx, nil, "", "eu", catalog.URL, "login", catalog.Client()); err != nil || got != "https://eu.api.entire.io" {
		t.Fatalf("no data host: got %q, %v (want catalog-resolved cell, never the core)", got, err)
	}
}

// TestNewEntireAPICellClient_TargetRoutesToRepoCell proves the repo-scoped path:
// when a CellTarget names a different jurisdiction than the caller's home, the
// client dials the TARGET cell with the login JWT — not the caller's home cell.
func TestNewEntireAPICellClient_TargetRoutesToRepoCell(t *testing.T) {
	isolateCellClientEnv(t, "https://entire.io")

	var exchangeHit bool
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == clustersAPIPath {
			t.Errorf("target path must not resolve clusters, but /api/v1/clusters was called")
		}
		if r.URL.Path == oauthTokenPath {
			exchangeHit = true
		}
		http.NotFound(w, r)
	}))
	defer coreSrv.Close()

	var euCellHit bool
	var gotAuthorization string
	euCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		euCellHit = true
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"repos":[]}`)
	}))
	defer euCell.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}
	t.Cleanup(SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	}))
	t.Cleanup(SetCellExchangeTransportForTest(t, coreSrv.Client().Transport))

	// Caller home_jurisdiction is "us"; the repo is homed in "eu".
	target := &CellTarget{BaseURL: euCell.URL, Jurisdiction: "eu"}
	client, err := NewEntireAPICellClient(context.Background(), false, target)
	if err != nil {
		t.Fatalf("NewEntireAPICellClient: %v", err)
	}
	if exchangeHit {
		t.Fatal("target path exchanged the login token")
	}

	resp, err := client.Get(context.Background(), "/api/v1/repos")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if !euCellHit {
		t.Fatal("request did not reach the target (eu) cell")
	}
	if gotAuthorization != "Bearer "+loginJWT {
		t.Fatalf("Authorization = %q, want login JWT bearer", gotAuthorization)
	}
}

// TestCellClientFactory_UsesLoginJWTDirectly pins the factory's credential
// contract: cell routing still follows the target, but the resolved login JWT
// is attached directly without a jurisdiction-token exchange.
// Not parallel: manipulates env + token store.
func TestCellClientFactory_UsesLoginJWTDirectly(t *testing.T) {
	isolateCellClientEnv(t, "https://entire.io")

	var wantLoginJWT string
	cellSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantLoginJWT {
			t.Errorf("Authorization = %q, want login JWT bearer", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cellSrv.Close()

	coreSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected jurisdiction-token exchange")
	}))
	defer coreSrv.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	wantLoginJWT = loginJWT
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}
	t.Cleanup(SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	}))

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}

	client, err := factory.ClientFor(context.Background(), &CellTarget{BaseURL: cellSrv.URL, Jurisdiction: "eu"})
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	resp, err := client.Get(context.Background(), "/api/v1/repos")
	if err != nil {
		t.Fatalf("cell request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

// catalogTransport serves GET /api/v1/clusters for one core host from a canned
// listing and 404s everything else, recording the catalog requests it sees. It
// stands in for a real (https) core so the cell path can be tested with
// production-shaped context CoreURLs.
type catalogTransport struct {
	coreHost string
	listing  string
	clusters []*http.Request
}

func (c *catalogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != clustersAPIPath {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	c.clusters = append(c.clusters, req)
	if req.URL.Host != c.coreHost {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.listing)),
		Request:    req,
	}, nil
}

// envFixture is one saved login environment: the context name, its core, and
// its core's cluster catalog.
type envFixture struct {
	name, coreURL, catalog string
}

func (f envFixture) coreHost() string {
	host, ok := hostOf(f.coreURL)
	if !ok {
		panic("envFixture coreURL has no host: " + f.coreURL)
	}
	return host
}

const (
	prodCoreURL    = "https://us.auth.entire.io"
	stagingCoreURL = "https://us.auth.partial.to"
)

var (
	prodFixture = envFixture{name: "me@entire", coreURL: prodCoreURL, catalog: `{"clusters":[` +
		`{"slug":"pedigree","jurisdiction":"us","isDefault":true,"apiUrl":"https://aws-us-east-2.api.entire.io"},` +
		`{"slug":"whiskas","jurisdiction":"eu","isDefault":true,"apiUrl":"https://aws-eu-west-1.api.entire.io"}]}`}
	stagingFixture = envFixture{name: "me@partial", coreURL: stagingCoreURL, catalog: `{"clusters":[` +
		`{"slug":"royalcanin","jurisdiction":"us","isDefault":true,"apiUrl":"https://aws-us-west-2.api.partial.to"},` +
		`{"slug":"eukanuba","jurisdiction":"eu","isDefault":true,"apiUrl":"https://aws-eu-west-1.api.partial.to"}]}`}
)

// seedProdAndStagingContexts saves the prod and staging login contexts with
// `current` as current_context, each with a fresh login JWT in the token
// store, and returns both JWTs.
func seedProdAndStagingContexts(t *testing.T, configDir, current string) (prodJWT, stagingJWT string) {
	t.Helper()
	var saved []*contexts.Context
	jwts := map[string]string{}
	for _, f := range []envFixture{prodFixture, stagingFixture} {
		svc := tokenstore.CoreKeyringService(f.coreURL)
		jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, f.coreURL, time.Now().Add(2*time.Hour).Unix()))
		if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
			t.Fatalf("seed token: %v", err)
		}
		jwts[f.name] = jwt
		saved = append(saved, &contexts.Context{Name: f.name, CoreURL: f.coreURL, Handle: "me", KeychainService: svc})
	}
	if err := contexts.Save(configDir, &contexts.File{CurrentContext: current, Contexts: saved}); err != nil {
		t.Fatalf("save contexts: %v", err)
	}
	return jwts[prodFixture.name], jwts[stagingFixture.name]
}

// isolateCellClientEnv pins every knob that steers cell routing: baseURL is
// the ENTIRE_API_BASE_URL to set ("" = unset, so the production default must
// not stand in for the selected context), templates and env token cleared, a
// fresh config dir and token store. With no data-host override the discovery
// seam FAILS the test if consulted — there is nothing to discover against.
func isolateCellClientEnv(t *testing.T, baseURL string) string {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	t.Setenv("ENTIRE_API_BASE_URL", baseURL)
	t.Setenv("ENTIRE_CONTEXT", "")
	// t.Setenv registers the restore; a blank ENTIRE_TOKEN is "set but blank"
	// (fail-closed), so it must be absent rather than empty.
	t.Setenv(EnvTokenVar, "")
	os.Unsetenv(EnvTokenVar)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	if baseURL == "" {
		t.Cleanup(SetResolveContextForCellAPIForTest(t, func(_ context.Context, _, _, host string, _ *http.Client, _ clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			t.Errorf("data-host discovery against %q must not run when ENTIRE_API_BASE_URL is unset", host)
			return nil, errors.New("unexpected discovery")
		}))
	}
	return configDir
}

// TestCellClientFactory_CellBaseURLFollowsSelectedContext pins COR-1634: the
// cell apiUrl for `entire api --to cell` / `-j <slug>` must come from the
// cluster catalog of the SELECTED context's core — current, $ENTIRE_CONTEXT, or
// --context — for prod and staging alike. Resolving it from the default data
// host (entire.io) aims every login at production and refuses a staging
// (partial.to) one outright. Not parallel: env + process-wide context override.
func TestCellClientFactory_CellBaseURLFollowsSelectedContext(t *testing.T) {
	prod, staging := prodFixture.name, stagingFixture.name
	tests := []struct {
		name         string
		current      string
		envContext   string
		flagContext  string
		jurisdiction string // "" = home
		wantCell     string
	}{
		{"prod current, home cell", prod, "", "", "", "https://aws-us-east-2.api.entire.io"},
		{"prod current, -j eu", prod, "", "", "eu", "https://aws-eu-west-1.api.entire.io"},
		{"staging current, home cell", staging, "", "", "", "https://aws-us-west-2.api.partial.to"},
		{"staging current, -j us", staging, "", "", "us", "https://aws-us-west-2.api.partial.to"},
		{"staging current, -j eu", staging, "", "", "eu", "https://aws-eu-west-1.api.partial.to"},
		{"prod current, $ENTIRE_CONTEXT staging", prod, staging, "", "", "https://aws-us-west-2.api.partial.to"},
		{"prod current, --context staging, -j eu", prod, "", staging, "eu", "https://aws-eu-west-1.api.partial.to"},
		{"staging current, --context prod", staging, "", prod, "", "https://aws-us-east-2.api.entire.io"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configDir := isolateCellClientEnv(t, "")
			prodJWT, stagingJWT := seedProdAndStagingContexts(t, configDir, tc.current)
			if tc.envContext != "" {
				t.Setenv("ENTIRE_CONTEXT", tc.envContext)
			}
			if tc.flagContext != "" {
				contexts.SetFlagOverrideForTest(t, tc.flagContext)
			}
			selected := cmp.Or(tc.flagContext, tc.envContext, tc.current)
			want, wantJWT := prodFixture, prodJWT
			if selected == staging {
				want, wantJWT = stagingFixture, stagingJWT
			}
			rt := &catalogTransport{coreHost: want.coreHost(), listing: want.catalog}
			t.Cleanup(SetCellExchangeTransportForTest(t, rt))

			factory, err := NewEntireAPICellClientFactory(context.Background(), false)
			if err != nil {
				t.Fatalf("NewEntireAPICellClientFactory: %v", err)
			}
			var target *CellTarget
			if tc.jurisdiction != "" {
				target = &CellTarget{Jurisdiction: tc.jurisdiction}
			}
			got, err := factory.cellBaseURLFor(context.Background(), target)
			if err != nil {
				t.Fatalf("cellBaseURLFor: %v", err)
			}
			if got != tc.wantCell {
				t.Fatalf("cell base URL = %q, want %q", got, tc.wantCell)
			}
			if len(rt.clusters) != 1 {
				t.Fatalf("clusters listed %d times, want exactly once", len(rt.clusters))
			}
			if got := rt.clusters[0].URL.Host; got != want.coreHost() {
				t.Errorf("clusters listed at %q, want the selected context's core %q", got, want.coreHost())
			}
			if got := rt.clusters[0].Header.Get("Authorization"); got != "Bearer "+wantJWT {
				t.Errorf("clusters Authorization = %q, want the selected context's login JWT", got)
			}
		})
	}
}

// TestCellClientFactory_UnknownJurisdictionNamesEnvironment: `-j` naming a
// jurisdiction the selected environment has no cell for fails with an error
// that says which core was consulted and which jurisdictions it does serve, and
// still unwraps to ErrNoCellForJurisdiction for callers that fall back on it.
func TestCellClientFactory_UnknownJurisdictionNamesEnvironment(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	seedProdAndStagingContexts(t, configDir, stagingFixture.name)
	rt := &catalogTransport{coreHost: stagingFixture.coreHost(), listing: `{"clusters":[` +
		`{"slug":"royalcanin","jurisdiction":"us","isDefault":true,"apiUrl":"https://aws-us-west-2.api.partial.to"},` +
		`{"slug":"pal","jurisdiction":"au","isDefault":true,"apiUrl":"https://aws-ap-southeast-2.api.partial.to"}]}`}
	t.Cleanup(SetCellExchangeTransportForTest(t, rt))

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}
	_, err = factory.cellBaseURLFor(context.Background(), &CellTarget{Jurisdiction: "eu"})
	if err == nil {
		t.Fatal("expected an error for a jurisdiction with no cell")
	}
	if !errors.Is(err, ErrNoCellForJurisdiction) {
		t.Errorf("error does not unwrap to ErrNoCellForJurisdiction: %v", err)
	}
	for _, want := range []string{`"eu"`, stagingCoreURL, "au, us"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

// TestCellClientFactory_NoActiveContextIsNotLoggedIn: with no ENTIRE_API_BASE_URL
// and no selected login, the cell path reports "not logged in" exactly as
// `--to core` does, instead of discovering a login against the production host.
func TestCellClientFactory_NoActiveContextIsNotLoggedIn(t *testing.T) {
	isolateCellClientEnv(t, "")
	_, err := NewEntireAPICellClientFactory(context.Background(), false)
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

// TestCellClientFactory_DataHostOverrideStillDiscovers: an explicit
// ENTIRE_API_BASE_URL is the user pointing the CLI at a data host, so the
// discovery path (trusted-issuer validation against THAT host) is kept.
func TestCellClientFactory_DataHostOverrideStillDiscovers(t *testing.T) {
	configDir := isolateCellClientEnv(t, "https://partial.to")
	_, stagingJWT := seedProdAndStagingContexts(t, configDir, prodFixture.name)

	discoveredHost := ""
	stagingSvc := tokenstore.CoreKeyringService(stagingCoreURL)
	t.Cleanup(SetResolveContextForCellAPIForTest(t, func(_ context.Context, _, _, host string, _ *http.Client, _ clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		discoveredHost = host
		return &contexts.Context{Name: stagingFixture.name, CoreURL: stagingCoreURL, Handle: "me", KeychainService: stagingSvc}, nil
	}))
	rt := &catalogTransport{coreHost: stagingFixture.coreHost(), listing: stagingFixture.catalog}
	t.Cleanup(SetCellExchangeTransportForTest(t, rt))

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}
	if discoveredHost != "partial.to" {
		t.Fatalf("discovery ran against %q, want the overridden data host partial.to", discoveredHost)
	}
	got, err := factory.cellBaseURLFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("cellBaseURLFor: %v", err)
	}
	if got != "https://aws-us-west-2.api.partial.to" {
		t.Fatalf("cell base URL = %q, want the discovered context's us cell", got)
	}
	if len(rt.clusters) != 1 || rt.clusters[0].Header.Get("Authorization") != "Bearer "+stagingJWT {
		t.Fatalf("clusters listing should carry the discovered context's JWT; requests: %d", len(rt.clusters))
	}
}

// TestCellClientFactory_LoopbackLoginResolvesCellFromCatalog: a local-dev login
// (core on loopback, no ENTIRE_API_BASE_URL) must reach the cell its core
// advertises, not the core itself — the core is a login server and 404s /me/*.
func TestCellClientFactory_LoopbackLoginResolvesCellFromCatalog(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	var clustersHit int
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != clustersAPIPath {
			http.NotFound(w, r)
			return
		}
		clustersHit++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"clusters":[{"jurisdiction":"us","isDefault":true,"apiUrl":"http://127.0.0.1:8082"}]}`)
	}))
	defer core.Close()

	svc := tokenstore.CoreKeyringService(core.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, core.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	writeActiveContext(t, configDir, "me@local", core.URL, "me", svc)

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}
	got, err := factory.cellBaseURLFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("cellBaseURLFor: %v", err)
	}
	if got != "http://127.0.0.1:8082" {
		t.Fatalf("cell base URL = %q, want the catalog's local cell, never the core %s", got, core.URL)
	}
	if clustersHit != 1 {
		t.Fatalf("clusters listed %d times, want once", clustersHit)
	}
}

// TestCellClientFactory_EnvTokenActsLikeCore: ENTIRE_TOKEN is honoured by the
// cell path exactly as by `--to core` — used verbatim as the login, its aud
// core's catalog consulted, no stored context or discovery needed.
func TestCellClientFactory_EnvTokenActsLikeCore(t *testing.T) {
	isolateCellClientEnv(t, "") // empty config dir: any stored-login path would fail "not logged in"
	envToken := makeJWT(t, fmt.Sprintf(`{"aud":%q,"home_jurisdiction":"us","exp":%d}`, stagingCoreURL, time.Now().Add(2*time.Hour).Unix()))
	t.Setenv(EnvTokenVar, envToken)
	rt := &catalogTransport{coreHost: stagingFixture.coreHost(), listing: stagingFixture.catalog}
	t.Cleanup(SetCellExchangeTransportForTest(t, rt))

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}
	got, err := factory.cellBaseURLFor(context.Background(), &CellTarget{Jurisdiction: "eu"})
	if err != nil {
		t.Fatalf("cellBaseURLFor: %v", err)
	}
	if got != "https://aws-eu-west-1.api.partial.to" {
		t.Fatalf("cell base URL = %q, want the env token's environment (staging) eu cell", got)
	}
	if len(rt.clusters) != 1 || rt.clusters[0].Header.Get("Authorization") != "Bearer "+envToken {
		t.Fatalf("clusters listing must carry the env token verbatim; requests: %d", len(rt.clusters))
	}
}

// TestCellClientFactory_EnvTokenHonoursExplicitDataHost: ENTIRE_TOKEN decides
// the login, but an explicit ENTIRE_API_BASE_URL still decides the host, with
// the same rules as a stored login — a direct cell or loopback dev server is
// dialed verbatim, an apex goes to the token's core catalog. Without this a
// pasted token plus a local data host sent the request to the token's home
// cell in AWS (trail finding on #2414).
func TestCellClientFactory_EnvTokenHonoursExplicitDataHost(t *testing.T) {
	tests := []struct {
		name         string
		baseURL      string
		insecure     bool
		wantCell     string
		wantCatalogs int
	}{
		{"loopback dev host", "http://127.0.0.1:8082", true, "http://127.0.0.1:8082", 0},
		{"direct cell", "https://aws-eu-west-1.api.partial.to", false, "https://aws-eu-west-1.api.partial.to", 0},
		{"apex resolves via the token's catalog", "https://partial.to", false, "https://aws-us-west-2.api.partial.to", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateCellClientEnv(t, tc.baseURL)
			envToken := makeJWT(t, fmt.Sprintf(`{"aud":%q,"home_jurisdiction":"us","exp":%d}`, stagingCoreURL, time.Now().Add(2*time.Hour).Unix()))
			t.Setenv(EnvTokenVar, envToken)
			rt := &catalogTransport{coreHost: stagingFixture.coreHost(), listing: stagingFixture.catalog}
			t.Cleanup(SetCellExchangeTransportForTest(t, rt))
			if tc.insecure {
				insecureHTTPOverride.Store(true)
				t.Cleanup(func() { insecureHTTPOverride.Store(false) })
			}

			factory, err := NewEntireAPICellClientFactory(context.Background(), tc.insecure)
			if err != nil {
				t.Fatalf("NewEntireAPICellClientFactory: %v", err)
			}
			got, err := factory.cellBaseURLFor(context.Background(), nil)
			if err != nil {
				t.Fatalf("cellBaseURLFor: %v", err)
			}
			if got != tc.wantCell {
				t.Fatalf("cell base URL = %q, want %q", got, tc.wantCell)
			}
			if len(rt.clusters) != tc.wantCatalogs {
				t.Fatalf("clusters listed %d times, want %d", len(rt.clusters), tc.wantCatalogs)
			}
		})
	}
}
