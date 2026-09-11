package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/entireio/auth-go/sts"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

func TestInfoFlagText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		flag     string
		want     bool
		contains []string
	}{
		{"version", "--version", true, []string{"git-remote-entire 1.2.3", "Go version:", "OS/Arch:"}},
		{"help", "--help", true, []string{"git-remote-entire 1.2.3", "entire://", "https://github.com/entireio/cli"}},
		{"unknown flag", "--nope", false, nil},
		{"empty", "", false, nil},
		{"url-like arg", "entire://host/p/r", false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, ok := infoFlagText(tc.flag, "1.2.3")
			if ok != tc.want {
				t.Fatalf("infoFlagText(%q) ok = %v, want %v", tc.flag, ok, tc.want)
			}
			if !ok {
				if text != "" {
					t.Fatalf("expected empty text when not handled, got %q", text)
				}
				return
			}
			for _, sub := range tc.contains {
				if !strings.Contains(text, sub) {
					t.Errorf("infoFlagText(%q) = %q, missing %q", tc.flag, text, sub)
				}
			}
		})
	}
}

func TestParseProtocolVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      string
		want     int
		wantWarn string
	}{
		{"unset", "", 2, ""},
		{"version_0", "version=0", 0, ""},
		{"version_1", "version=1", 1, ""},
		{"version_2", "version=2", 2, ""},
		{"unknown_version_warns", "version=3", 2, "ignoring unrecognised protocol.version"},
		{"malformed_value_warns", "version=abc", 2, "ignoring unrecognised protocol.version"},
		{"empty_value_warns", "version=", 2, "ignoring unrecognised protocol.version"},
		{"no_version_key", "foo=bar", 2, ""},
		{"version_after_other_key", "foo=bar:version=1", 1, ""},
		{"version_before_other_key", "version=2:foo=bar", 2, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			got := parseProtocolVersion(tc.env, &buf)
			if got != tc.want {
				t.Errorf("parseProtocolVersion(%q) = %d, want %d", tc.env, got, tc.want)
			}
			switch {
			case tc.wantWarn == "" && buf.Len() != 0:
				t.Errorf("expected no warning, got %q", buf.String())
			case tc.wantWarn != "" && !strings.Contains(buf.String(), tc.wantWarn):
				t.Errorf("expected warning containing %q, got %q", tc.wantWarn, buf.String())
			}
		})
	}
}

func TestGitActionFromRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		path   string
		query  string
		want   string
	}{
		{"upload-pack RPC", http.MethodPost, "/et/p/r/git-upload-pack", "", "pull"},
		{"receive-pack RPC", http.MethodPost, "/et/p/r/git-receive-pack", "", "push"},
		{"info/refs pull", http.MethodGet, "/et/p/r/info/refs", "service=git-upload-pack", "pull"},
		{"info/refs push", http.MethodGet, "/et/p/r/info/refs", "service=git-receive-pack", "push"},
		{"info/refs no service", http.MethodGet, "/et/p/r/info/refs", "", ""},
		{"unrelated GET", http.MethodGet, "/et/p/r/objects/info/packs", "", ""},
		{"unrelated POST", http.MethodPost, "/et/p/r/whatever", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(context.Background(), tc.method, "https://host"+tc.path+"?"+tc.query, nil)
			if got := gitActionFromRequest(req); got != tc.want {
				t.Fatalf("gitActionFromRequest(%s %s?%s) = %q, want %q", tc.method, tc.path, tc.query, got, tc.want)
			}
		})
	}
}

func TestFatalMessage(t *testing.T) {
	t.Parallel()
	parsedURL := &url.URL{Scheme: "entire", Host: "aws-us-east-2.entire.io", Path: "/et/paul/dogbark"}
	wrongCluster := &sts.ExchangeError{
		StatusCode:  http.StatusBadRequest,
		Code:        "invalid_target",
		Description: `audience host "aws-us-east-2.entire.io" does not host this repo; it lives on "aws-eu-central-1.entire.io" — re-target the request there`,
	}
	tests := []struct {
		name        string
		err         error
		contains    []string
		notContains []string
	}{
		{
			name: "wrong cluster names correct host and URL",
			// Wrapped to mirror production: the ExchangeError surfaces buried under
			// several fmt.Errorf layers, so errors.As must dig it out.
			err: fmt.Errorf("stateless-connect v2 info/refs: fetching info/refs from entry domain: repo-scoped token exchange: oauth token exchange: %w", wrongCluster),
			contains: []string{
				"aws-eu-central-1.entire.io",
				"git clone entire://aws-eu-central-1.entire.io/et/paul/dogbark",
			},
			notContains: []string{"HTTP 400", "invalid_target"},
		},
		{
			name:     "invalid_target without lives-on hint falls back",
			err:      &sts.ExchangeError{StatusCode: http.StatusBadRequest, Code: "invalid_target", Description: "no servable mirror"},
			contains: []string{"fatal:", "no servable mirror"},
		},
		{
			name:     "unrelated error falls back verbatim",
			err:      errors.New("connection refused"),
			contains: []string{"fatal: connection refused"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fatalMessage(tc.err, parsedURL)
			for _, sub := range tc.contains {
				if !strings.Contains(got, sub) {
					t.Errorf("fatalMessage() = %q, missing %q", got, sub)
				}
			}
			for _, sub := range tc.notContains {
				if strings.Contains(got, sub) {
					t.Errorf("fatalMessage() = %q, should not contain %q", got, sub)
				}
			}
		})
	}
}

func TestMissingClusterHostMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rawURL      string
		contains    []string
		notContains []string
	}{
		{
			// The motivating case: forge id typed where the cluster host belongs.
			name:     "forge id in host slot points at repo clone",
			rawURL:   "entire://gh/entire.io/cli",
			contains: []string{"missing its cluster host", `"gh" is a forge id`, "entire repo clone /gh/entire.io/cli"},
		},
		{
			// Empty host but the path already reads as a forge shorthand.
			name:     "empty host with forge path points at repo clone",
			rawURL:   "entire:///gh/entire.io/cli",
			contains: []string{"missing its cluster host", "entire repo clone /gh/entire.io/cli"},
		},
		{
			// Empty host, leading segment is not a known forge → generic error.
			name:        "empty host with non-forge path falls back",
			rawURL:      "entire:///not-a-forge/owner/repo",
			contains:    []string{`fatal: missing host in URL "entire:///not-a-forge/owner/repo"`},
			notContains: []string{"entire repo clone"},
		},
		{
			name:        "bare scheme falls back",
			rawURL:      "entire://",
			contains:    []string{`fatal: missing host in URL "entire://"`},
			notContains: []string{"entire repo clone"},
		},
		{
			// Not enough path to form owner/repo → not worth pointing at clone.
			name:        "empty host single-segment path falls back",
			rawURL:      "entire:///gh",
			contains:    []string{`fatal: missing host in URL "entire:///gh"`},
			notContains: []string{"entire repo clone"},
		},
		{
			// Forge in host slot but no owner/repo — the shorthand `/gh` would be
			// rejected by `entire repo clone`, so fall back rather than suggest it.
			name:        "forge in host slot without path falls back",
			rawURL:      "entire://gh",
			contains:    []string{`fatal: missing host in URL "entire://gh"`},
			notContains: []string{"entire repo clone"},
		},
		{
			// Forge in host slot with owner but no repo — incomplete triple.
			name:        "forge in host slot with owner only falls back",
			rawURL:      "entire://gh/owner",
			contains:    []string{`fatal: missing host in URL "entire://gh/owner"`},
			notContains: []string{"entire repo clone"},
		},
		{
			// Empty host, forge + owner but no repo — incomplete triple.
			name:        "empty host forge and owner only falls back",
			rawURL:      "entire:///gh/owner",
			contains:    []string{`fatal: missing host in URL "entire:///gh/owner"`},
			notContains: []string{"entire repo clone"},
		},
		{
			// Too many segments — not the gh/<owner>/<repo> shape either.
			name:        "forge in host slot with extra path segment falls back",
			rawURL:      "entire://gh/owner/repo/extra",
			contains:    []string{`fatal: missing host in URL "entire://gh/owner/repo/extra"`},
			notContains: []string{"entire repo clone"},
		},
		{
			// The native forge token is recognized in the host slot too, so it
			// gets the actionable message rather than an attempt to dial a
			// cluster literally named "et" — and its segments are labelled
			// <project>/<repo>, which is what /et/ paths actually take.
			name:   "native forge in host slot is actionable",
			rawURL: "entire://et/paul/dogbark",
			contains: []string{
				`("et" is a forge id, not a host)`,
				"entire://<cluster-host>/et/<project>/<repo>",
				"entire repo clone /et/paul/dogbark",
			},
			notContains: []string{"<owner>/<repo>"},
		},
		{
			name:   "native forge with empty host is actionable",
			rawURL: "entire:///et/paul/dogbark",
			contains: []string{
				"entire://<cluster-host>/et/<project>/<repo>",
				"entire repo clone /et/paul/dogbark",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := url.Parse(tc.rawURL)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.rawURL, err)
			}
			got := missingClusterHostMessage(parsed, tc.rawURL)
			for _, sub := range tc.contains {
				if !strings.Contains(got, sub) {
					t.Errorf("missingClusterHostMessage(%q) = %q, missing %q", tc.rawURL, got, sub)
				}
			}
			for _, sub := range tc.notContains {
				if strings.Contains(got, sub) {
					t.Errorf("missingClusterHostMessage(%q) = %q, should not contain %q", tc.rawURL, got, sub)
				}
			}
		})
	}
}

func TestCoreTrusted(t *testing.T) {
	t.Parallel()
	trusted := []string{"https://core.us.entire.io", "https://core.eu.entire.io/"}
	tests := []struct {
		name    string
		coreURL string
		want    bool
	}{
		{"exact match", "https://core.us.entire.io", true},
		{"trailing slash on candidate", "https://core.us.entire.io/", true},
		{"trailing slash on trusted entry", "https://core.eu.entire.io", true},
		{"not in set", "https://attacker.example.com", false},
		{"empty against set", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := coreTrusted(tc.coreURL, trusted); got != tc.want {
				t.Fatalf("coreTrusted(%q) = %v, want %v", tc.coreURL, got, tc.want)
			}
		})
	}
}

func TestCoreTrusted_EmptyTrustedSet(t *testing.T) {
	t.Parallel()
	if coreTrusted("https://core.us.entire.io", nil) {
		t.Fatal("coreTrusted should be false against an empty trusted set")
	}
}

// makeTestJWT builds a three-segment JWT (alg:HS256 so ParseClaims accepts it)
// carrying the given aud. The signature segment is filler — the env-token path
// reads the aud unverified and gates it on cluster-advertised cores, never on
// the signature.
func makeTestJWT(t *testing.T, aud string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(fmt.Sprintf(`{"sub":"ci-runner","aud":%q}`, aud)))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

// testClusterHost is the cluster name the ENTIRE_TOKEN tests dial. It shares
// entire.io with the cores they advertise, which the same-site gate on
// discovery requires; the TLS test server itself answers on 127.0.0.1, so
// pinnedClient rewrites the dial while the URL the code sees keeps this name.
const testClusterHost = "cluster.entire.io"

// pinnedClient returns srv.Client() with every request redirected to srv,
// whatever host the URL names. TLS still verifies against 127.0.0.1, which
// the httptest certificate covers.
func pinnedClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := srv.Client()
	c.Transport = pinnedTransport{base: c.Transport, scheme: u.Scheme, host: u.Host}
	return c
}

type pinnedTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
}

func (p pinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = p.scheme
	req.URL.Host = p.host
	return p.base.RoundTrip(req)
}

// wellKnownServer serves /.well-known/entire-cluster.json advertising the
// given cores, jurisdiction audience, and jurisdiction core over TLS,
// returning a client pinned to it and testClusterHost to use as clusterHost.
// An empty audience models a cluster predating jurisdiction-token git auth.
func wellKnownServer(t *testing.T, cores []string, jurisdictionAudience, jurisdictionCoreURL string) (*http.Client, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/entire-cluster.json" {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{"core_urls": cores}
		if jurisdictionAudience != "" {
			body["jurisdiction_audience"] = jurisdictionAudience
		}
		if jurisdictionCoreURL != "" {
			body["jurisdiction_core_url"] = jurisdictionCoreURL
		}
		_ = json.NewEncoder(w).Encode(body) //nolint:errcheck // best-effort in test stub
	}))
	t.Cleanup(srv.Close)
	return pinnedClient(t, srv), testClusterHost
}

func TestResolveEnvTokenCreds_TrustedAudSucceeds(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	const audience = "https://us.entire.io"
	client, clusterHost := wellKnownServer(t, []string{core}, audience, core)
	envToken := makeTestJWT(t, core)

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), envToken, clusterHost, t.TempDir(), client,
	)
	if err != nil {
		t.Fatalf("expected trusted aud to succeed, got: %v", err)
	}
	got, err := creds(t.Context())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != envToken {
		t.Errorf("creds = %q, want ENTIRE_TOKEN verbatim", got)
	}
}

func TestResolveEnvTokenCreds_CrossJurisdictionTokenUsesBearer(t *testing.T) {
	t.Parallel()
	// Clusters advertise every jurisdiction's cores, so a token minted at a
	// sibling core passes the trust gate and is used directly as the bearer.
	const tokenCore = "https://core.eu.entire.io"
	const jurisdictionCore = "https://core.us.entire.io"
	client, clusterHost := wellKnownServer(t, []string{tokenCore, jurisdictionCore}, "https://us.entire.io", jurisdictionCore)
	envToken := makeTestJWT(t, tokenCore)

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), envToken, clusterHost, t.TempDir(), client,
	)
	if err != nil {
		t.Fatalf("cross-jurisdiction token must resolve, got: %v", err)
	}
	got, err := creds(t.Context())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != envToken {
		t.Errorf("creds = %q, want ENTIRE_TOKEN verbatim", got)
	}
}

func TestResolveEnvTokenCreds_DoesNotRequireJurisdictionAudience(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	client, clusterHost := wellKnownServer(t, []string{core}, "", "")
	envToken := makeTestJWT(t, core)

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), envToken, clusterHost, t.TempDir(), client,
	)
	if err != nil {
		t.Fatalf("resolve direct bearer without jurisdiction audience: %v", err)
	}
	got, err := creds(t.Context())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != envToken {
		t.Errorf("creds = %q, want ENTIRE_TOKEN verbatim", got)
	}
}

func TestResolveCreds_BlankEnvTokenFailsClosed(t *testing.T) {
	// If ENTIRE_TOKEN is set at all, presence commits us to the env-token path:
	// an empty or whitespace-only value must fail closed with a clear message,
	// never silently fall back to context auth. Sets a process-global env var,
	// so this test is not parallel.
	dummyURL := &url.URL{Scheme: "entire", Host: "cluster.example.com"}
	for _, blank := range []string{"", " ", "\t", "\n", " \t\n "} {
		t.Setenv(auth.EnvTokenVar, blank)
		creds, _, err := resolveCreds(t.Context(), dummyURL, false, nil)
		if err == nil {
			t.Fatalf("blank ENTIRE_TOKEN %q should fail closed", blank)
		}
		if creds != nil {
			t.Fatalf("expected nil creds for blank ENTIRE_TOKEN %q", blank)
		}
		if !strings.Contains(err.Error(), "blank") {
			t.Fatalf("expected 'set but blank' error for %q, got: %v", blank, err)
		}
	}
}

func TestSetAuthWithProvider_ResolvesCredentialPerRequest(t *testing.T) {
	t.Parallel()
	calls := 0
	setAuth := setAuthWithProvider(func(context.Context) (string, error) {
		calls++
		return fmt.Sprintf("login-jwt-%d", calls), nil
	})

	for i, want := range []string{"Bearer login-jwt-1", "Bearer login-jwt-2"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://cluster.example.com/et/alice/repo/info/refs?service=git-upload-pack", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := setAuth(req); err != nil {
			t.Fatalf("setAuth[%d]: %v", i, err)
		}
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization[%d] = %q, want %q", i, got, want)
		}
	}
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
}

type fakeRefreshableCredential struct {
	token       string
	forcedToken string
	tokenCalls  int
	forceCalls  int
	forcedStale string
}

func (f *fakeRefreshableCredential) Token(context.Context) (string, error) {
	f.tokenCalls++
	return f.token, nil
}

func (f *fakeRefreshableCredential) ForceRefresh(_ context.Context, staleToken string) (string, error) {
	f.forceCalls++
	f.forcedStale = staleToken
	f.token = f.forcedToken
	return f.token, nil
}

func TestRefreshingProvider_ForceRefreshesAfterUnauthorized(t *testing.T) {
	t.Parallel()
	source := &fakeRefreshableCredential{token: "rejected-jwt", forcedToken: "refreshed-jwt"}
	provider, onUnauthorized := refreshingProvider(source)

	got, err := provider(t.Context())
	if err != nil {
		t.Fatalf("initial provider: %v", err)
	}
	if got != "rejected-jwt" {
		t.Fatalf("initial token = %q, want rejected-jwt", got)
	}

	onUnauthorized()
	got, err = provider(t.Context())
	if err != nil {
		t.Fatalf("provider after 401: %v", err)
	}
	if got != "refreshed-jwt" {
		t.Fatalf("token after 401 = %q, want refreshed-jwt", got)
	}
	if source.forceCalls != 1 || source.forcedStale != "rejected-jwt" {
		t.Fatalf("ForceRefresh calls = %d with stale %q, want 1 with rejected-jwt", source.forceCalls, source.forcedStale)
	}
}

func TestResolveEnvTokenCreds_UntrustedAudAborts(t *testing.T) {
	t.Parallel()
	// The cluster advertises only core.us; the token's aud points elsewhere.
	// The gate must abort before building creds (i.e. before any exchange).
	client, clusterHost := wellKnownServer(t, []string{"https://core.us.entire.io"}, "https://us.entire.io", "https://core.us.entire.io")

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://attacker.example.com"), clusterHost, t.TempDir(), client,
	)
	if err == nil {
		t.Fatal("expected untrusted aud to be rejected")
	}
	if creds != nil {
		t.Fatal("expected nil creds when aud is untrusted")
	}
	if !strings.Contains(err.Error(), "not a trusted login server") {
		t.Fatalf("expected trust-gate error, got: %v", err)
	}
}

func TestResolveEnvTokenCreds_EmptyAdvertisedCoresAborts(t *testing.T) {
	t.Parallel()
	// Discovery succeeds (HTTP 200) but advertises no cores. With nothing to
	// trust, the gate must fail closed rather than trusting the token's aud.
	client, clusterHost := wellKnownServer(t, []string{}, "https://us.entire.io", "https://core.us.entire.io")

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://core.us.entire.io"), clusterHost, t.TempDir(), client,
	)
	if err == nil {
		t.Fatal("expected empty advertised core set to be rejected")
	}
	if creds != nil {
		t.Fatal("expected nil creds when no cores are advertised")
	}
}

func TestResolveEnvTokenCreds_CrossSiteCoresAbort(t *testing.T) {
	t.Parallel()
	// A cluster under one domain advertising a core under another is the
	// shape of a token-stealing cluster: the token's aud WOULD match the
	// advertised list, so coreTrusted alone would hand it over. The same-site
	// gate on discovery must refuse first, naming both sides.
	const foreignCore = "https://core.us.evil.example"
	client, clusterHost := wellKnownServer(t, []string{foreignCore}, "https://us.entire.io", foreignCore)

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, foreignCore), clusterHost, t.TempDir(), client,
	)
	if err == nil {
		t.Fatal("expected a cross-site core to be refused")
	}
	if creds != nil {
		t.Fatal("expected nil creds when a cross-site core is advertised")
	}
	for _, want := range []string{"cluster " + testClusterHost, foreignCore, "outside entire.io", "refusing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "not a trusted login server") {
		t.Fatal("the same-site gate must refuse before the aud comparison runs")
	}
}

func TestResolveEnvTokenCreds_DiscoveryFailureAborts(t *testing.T) {
	t.Parallel()
	// Cluster advertises no cores (HTTP 503) → discovery fails → we must abort
	// rather than fall back to trusting the token's own aud.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://core.us.entire.io"), u.Host, t.TempDir(), srv.Client(),
	)
	if err == nil {
		t.Fatal("expected discovery failure to abort")
	}
	if creds != nil {
		t.Fatal("expected nil creds on discovery failure")
	}
}

func TestResolveEnvTokenCreds_MalformedTokenAborts(t *testing.T) {
	t.Parallel()
	// A malformed aud must fail at the parse/validate step, before any network
	// discovery happens — so a nil httpClient is safe here.
	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "http://core.us.entire.io"), "cluster.example.com", t.TempDir(), nil,
	)
	if err == nil {
		t.Fatal("expected http aud to be rejected before discovery")
	}
	if creds != nil {
		t.Fatal("expected nil creds for invalid aud")
	}
}
