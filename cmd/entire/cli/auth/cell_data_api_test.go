package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// usEntireAudience is the prod "us" jurisdiction audience, reused across the
// cell/jurisdiction tests.
const usEntireAudience = "https://us.entire.io"

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

func TestEntireDomainFamily(t *testing.T) {
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
		if got := entireDomainFamily(tc.core); got != tc.want {
			t.Errorf("entireDomainFamily(%q) = %q, want %q", tc.core, got, tc.want)
		}
	}
}

func TestJurisdictionAudienceFollowsLoginFamily(t *testing.T) {
	// No env override: the audience must follow the environment family so a
	// staging (partial.to) login mints a partial.to audience, not a prod one.
	t.Setenv("ENTIRE_API_AUDIENCE_TEMPLATE", "")
	if got := jurisdictionAudience("us", "https://entire.io", "https://us.auth.entire.io"); got != usEntireAudience {
		t.Errorf("prod audience = %q, want https://us.entire.io", got)
	}
	if got := jurisdictionAudience("eu", "https://partial.to", "https://us.auth.partial.to"); got != "https://eu.partial.to" {
		t.Errorf("staging audience = %q, want https://eu.partial.to", got)
	}
}

func TestJurisdictionCoreURLHonorsLoopbackAndFamily(t *testing.T) {
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "")
	// Local dev: a loopback discovered core must be honored verbatim, NOT
	// replaced by the production template (which would send the local login JWT
	// to prod).
	if got := jurisdictionCoreURL("us", "http://127.0.0.1:8099", "http://127.0.0.1:9000"); got != "http://127.0.0.1:9000" {
		t.Errorf("loopback core = %q, want http://127.0.0.1:9000", got)
	}
	// Staging: core follows the environment family and target jurisdiction.
	if got := jurisdictionCoreURL("eu", "https://partial.to", "https://us.auth.partial.to"); got != "https://eu.auth.partial.to" {
		t.Errorf("staging core = %q, want https://eu.auth.partial.to", got)
	}
	// Prod: mirrors the audience test's prod/staging pair.
	if got := jurisdictionCoreURL("eu", "https://entire.io", "https://us.auth.entire.io"); got != "https://eu.auth.entire.io" {
		t.Errorf("prod core = %q, want https://eu.auth.entire.io", got)
	}
}

func TestJurisdictionCoreURLHonorsFixedTemplate(t *testing.T) {
	// A placeholder-less template names a single core for every jurisdiction
	// (single-core deployments), matching the BFF and the audience handler.
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "https://single-core.example")
	if got := jurisdictionCoreURL("eu", "https://entire.io", "https://us.auth.entire.io"); got != "https://single-core.example" {
		t.Errorf("fixed-template core = %q, want https://single-core.example", got)
	}
	// A loopback discovered core still wins over any template (local dev).
	if got := jurisdictionCoreURL("eu", "https://entire.io", "http://127.0.0.1:9000"); got != "http://127.0.0.1:9000" {
		t.Errorf("loopback core = %q, want http://127.0.0.1:9000", got)
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
		{usEntireAudience, false},
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
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "https://fixed-core.test")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const wantAudience = usEntireAudience

	var gotExchangeAudience, gotReposHost string
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
		case oauthTokenPath:
			_ = r.ParseForm() //nolint:errcheck // test handler
			gotExchangeAudience = r.FormValue("audience")
			if r.FormValue("scope") != JurisdictionIdentityScope {
				t.Errorf("scope = %q, want openid", r.FormValue("scope"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"cell-identity-token","token_type":"Bearer","expires_in":3600}`)
		case "/api/v1/repos":
			gotReposHost = r.Host
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
	if gotExchangeAudience != wantAudience {
		t.Fatalf("exchange audience = %q, want %q", gotExchangeAudience, wantAudience)
	}

	resp, err := client.Get(context.Background(), "/api/v1/repos")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if gotReposHost == "" {
		t.Fatal("cell repos request was not received")
	}
	auth := resp.Request.Header.Get("Authorization")
	if !strings.Contains(auth, "cell-identity-token") {
		t.Fatalf("Authorization = %q, want cell identity token", auth)
	}
}

func TestNewEntireAPICellClient_KeepsDirectCellBaseURL(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	const cellBase = "https://aws-us-east-2.api.entire.io"
	t.Setenv("ENTIRE_API_BASE_URL", cellBase)
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "https://fixed-core.test")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

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
	// A direct cell origin must NOT trigger cluster resolution, but the identity
	// token must still be minted.
	if clustersHit {
		t.Error("direct cell URL should not resolve clusters")
	}
	if !exchangeHit {
		t.Error("identity token exchange did not happen for a direct cell URL")
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
}

// TestNewEntireAPICellClient_TargetRoutesToRepoCell proves the repo-scoped path:
// when a CellTarget names a different jurisdiction than the caller's home, the
// client mints for the TARGET jurisdiction and dials the TARGET cell — not the
// caller's home cell. This is the cross-jurisdiction case the home-only routing
// could never satisfy.
func TestNewEntireAPICellClient_TargetRoutesToRepoCell(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	t.Setenv("ENTIRE_API_AUDIENCE_TEMPLATE", "")
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var gotExchangeAudience string
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == clustersAPIPath {
			t.Errorf("target path must not resolve clusters, but /api/v1/clusters was called")
		}
		if r.URL.Path != oauthTokenPath {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm() //nolint:errcheck // test handler
		gotExchangeAudience = r.FormValue("audience")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"eu-identity-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer coreSrv.Close()

	var euCellHit bool
	euCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		euCellHit = true
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
	if gotExchangeAudience != "https://eu.entire.io" {
		t.Fatalf("exchange audience = %q, want https://eu.entire.io (repo jurisdiction, not caller home us)", gotExchangeAudience)
	}

	resp, err := client.Get(context.Background(), "/api/v1/repos")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if !euCellHit {
		t.Fatal("request did not reach the target (eu) cell")
	}
	if got := resp.Request.Header.Get("Authorization"); !strings.Contains(got, "eu-identity-token") {
		t.Fatalf("Authorization = %q, want eu identity token", got)
	}
}

// TestJurisdictionToken_StoredContext exercises the exported token-only path off
// a stored login context: it must return the exchanged identity token and mint
// it with scope=openid, the jurisdiction audience, and the login JWT as
// subject_token. Not parallel: manipulates env + token store.
func TestJurisdictionToken_StoredContext(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var gotAudience, gotScope, gotSubject, gotGrant string
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oauthTokenPath {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm() //nolint:errcheck // test handler
		gotAudience = r.FormValue("audience")
		gotScope = r.FormValue("scope")
		gotSubject = r.FormValue("subject_token")
		gotGrant = r.FormValue("grant_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"cell-identity-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer coreSrv.Close()

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

	token, err := JurisdictionToken(context.Background(), false, "us")
	if err != nil {
		t.Fatalf("JurisdictionToken: %v", err)
	}
	if token != "cell-identity-token" {
		t.Fatalf("token = %q, want cell-identity-token", token)
	}
	if gotAudience != usEntireAudience {
		t.Errorf("audience = %q, want https://us.entire.io", gotAudience)
	}
	if gotScope != JurisdictionIdentityScope {
		t.Errorf("scope = %q, want %q", gotScope, JurisdictionIdentityScope)
	}
	if gotSubject != loginJWT {
		t.Errorf("subject_token = %q, want the login JWT", gotSubject)
	}
	if gotGrant != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Errorf("grant_type = %q, want token-exchange", gotGrant)
	}
}

// captureTransport counts exchanges and records the last request's parsed
// form body, URL, and Authorization header, returning a canned RFC 8693
// token-exchange success response. The minted access_token is `token`, or
// "repo-scoped.jwt" when unset.
type captureTransport struct {
	calls int
	form  url.Values
	url   string
	auth  string
	token string
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	c.calls++
	c.form = form
	c.url = req.URL.String()
	c.auth = req.Header.Get("Authorization")
	accessToken := c.token
	if accessToken == "" {
		accessToken = "repo-scoped.jwt"
	}
	resp := fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer",`+
		`"issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":300}`, accessToken)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(resp)),
		Request:    req,
	}, nil
}

// TestJurisdictionToken_EnvToken proves ENTIRE_TOKEN is used as the exchange
// subject with no stored context or discovery, and that its own aud drives the
// environment family (no ENTIRE_API_BASE_URL set). Not parallel: sets env.
func TestJurisdictionToken_EnvToken(t *testing.T) {
	// Empty config dir: if the env-token path fell through to stored-login
	// resolution this would fail "not logged in", so success proves the env path.
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	envToken := makeJWT(t, fmt.Sprintf(`{"aud":"https://us.auth.entire.io","home_jurisdiction":"us","exp":%d}`, time.Now().Add(2*time.Hour).Unix()))
	t.Setenv("ENTIRE_TOKEN", envToken)

	// captureTransport intercepts the /oauth/token POST and records the form, so
	// the ENTIRE_TOKEN path is tested without a real (https) core server.
	rt := &captureTransport{token: "env-cell-token"}
	t.Cleanup(SetCellExchangeTransportForTest(t, rt))

	// Explicit jurisdiction: audience follows the requested region, subject is the
	// env token verbatim.
	token, err := JurisdictionToken(context.Background(), false, "eu")
	if err != nil {
		t.Fatalf("JurisdictionToken(eu): %v", err)
	}
	if token != "env-cell-token" {
		t.Fatalf("token = %q, want env-cell-token", token)
	}
	if got := rt.form.Get("subject_token"); got != envToken {
		t.Errorf("subject_token = %q, want the ENTIRE_TOKEN value", got)
	}
	if got := rt.form.Get("audience"); got != "https://eu.entire.io" {
		t.Errorf("audience = %q, want https://eu.entire.io", got)
	}
	if got := rt.form.Get("scope"); got != JurisdictionIdentityScope {
		t.Errorf("scope = %q, want %q", got, JurisdictionIdentityScope)
	}

	// Empty jurisdiction falls back to the env token's home_jurisdiction claim.
	if _, err := JurisdictionToken(context.Background(), false, ""); err != nil {
		t.Fatalf("JurisdictionToken(home): %v", err)
	}
	if got := rt.form.Get("audience"); got != usEntireAudience {
		t.Errorf("home-fallback audience = %q, want https://us.entire.io", got)
	}
}

// TestCellClientFactory_ReusesTokenPerJurisdiction pins the factory's core
// contract: identity tokens are per-jurisdiction, not per-cell, so building
// clients for several cells must mint one token per distinct jurisdiction and
// reuse it across cells. Not parallel: manipulates env + token store.
func TestCellClientFactory_ReusesTokenPerJurisdiction(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	t.Setenv("ENTIRE_API_AUDIENCE_TEMPLATE", "")
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var exchangeCount int
	var audiences []string
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oauthTokenPath {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm() //nolint:errcheck // test handler
		exchangeCount++
		audiences = append(audiences, r.FormValue("audience"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"identity-token-%d","token_type":"Bearer","expires_in":3600}`, exchangeCount)
	}))
	defer coreSrv.Close()

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

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}

	// Two eu cells then one us cell: two exchanges total, the eu token reused
	// for the second eu cell.
	for _, target := range []*CellTarget{
		{BaseURL: "https://cell-a.api.example", Jurisdiction: "eu"},
		{BaseURL: "https://cell-b.api.example", Jurisdiction: "eu"},
		{BaseURL: "https://cell-c.api.example", Jurisdiction: "us"},
	} {
		if _, err := factory.ClientFor(context.Background(), target); err != nil {
			t.Fatalf("ClientFor(%s): %v", target.BaseURL, err)
		}
	}
	if exchangeCount != 2 {
		t.Fatalf("exchange count = %d, want 2 (one per distinct jurisdiction)", exchangeCount)
	}
	if got, want := strings.Join(audiences, ","), "https://eu.entire.io,"+usEntireAudience; got != want {
		t.Fatalf("exchange audiences = %q, want %q", got, want)
	}
}

// TestCellClientFactory_SingleFlightsConcurrentMints pins the per-jurisdiction
// locking: concurrent ClientFor calls for the same jurisdiction produce exactly
// one exchange (the rest wait on the slot and reuse the result), not one each.
// Not parallel: manipulates env + token store.
func TestCellClientFactory_SingleFlightsConcurrentMints(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	t.Setenv("ENTIRE_API_AUDIENCE_TEMPLATE", "")
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var mu sync.Mutex
	exchangeCount := 0
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oauthTokenPath {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		exchangeCount++
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // widen the window concurrent callers could race into
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"identity-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer coreSrv.Close()

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

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = factory.ClientFor(context.Background(), &CellTarget{
				BaseURL:      fmt.Sprintf("https://cell-%d.api.example", i),
				Jurisdiction: "eu",
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ClientFor[%d]: %v", i, err)
		}
	}
	if exchangeCount != 1 {
		t.Fatalf("exchange count = %d, want 1 (same jurisdiction single-flighted)", exchangeCount)
	}
}
