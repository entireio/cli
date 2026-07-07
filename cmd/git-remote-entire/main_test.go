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

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/httputil"
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
	wrongCluster := &httputil.OAuthError{
		Status:      http.StatusBadRequest,
		Code:        "invalid_target",
		Description: `audience host "aws-us-east-2.entire.io" does not host this repo; it lives on "aws-eu-central-1.entire.io" — re-target the request there`,
		Body:        "{...}",
	}
	tests := []struct {
		name        string
		err         error
		contains    []string
		notContains []string
	}{
		{
			name: "wrong cluster names correct host and URL",
			// Wrapped to mirror production: the OAuthError surfaces buried under
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
			err:      &httputil.OAuthError{Status: http.StatusBadRequest, Code: "invalid_target", Description: "no servable mirror", Body: "HTTP 400: no servable mirror"},
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

// wellKnownServer serves /.well-known/entire-cluster.json advertising the
// given cores, jurisdiction audience, and jurisdiction core over TLS,
// returning the server and the host:port to use as clusterHost. An empty
// audience models a cluster predating jurisdiction-token git auth.
func wellKnownServer(t *testing.T, cores []string, jurisdictionAudience, jurisdictionCoreURL string) (*httptest.Server, string) {
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
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return srv, u.Host
}

func TestResolveEnvTokenCreds_TrustedAudSucceeds(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	const audience = "https://us.entire.io"
	srv, clusterHost := wellKnownServer(t, []string{core}, audience, core)

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, core), clusterHost, t.TempDir(), srv.Client(),
	)
	if err != nil {
		t.Fatalf("expected trusted aud to succeed, got: %v", err)
	}
	if creds == nil {
		t.Fatal("expected non-nil creds for trusted aud")
	}
	// The exchange must be pinned to the token's own (trust-gated) core with
	// the cluster's advertised jurisdiction audience, and must never touch
	// the keychain — CI runners don't have one.
	if creds.fixedCoreURL != core {
		t.Errorf("fixedCoreURL = %q, want %q", creds.fixedCoreURL, core)
	}
	if creds.audience != audience {
		t.Errorf("audience = %q, want %q", creds.audience, audience)
	}
	if creds.persist {
		t.Error("env-token creds must not persist to the keychain")
	}
	if creds.exchangeHint != "" {
		t.Errorf("no cross-jurisdiction hint expected when the token's core is the jurisdiction core, got %q", creds.exchangeHint)
	}
}

func TestResolveEnvTokenCreds_CrossJurisdictionTokenGetsHint(t *testing.T) {
	t.Parallel()
	// Clusters advertise every jurisdiction's cores, so a token minted at a
	// sibling core passes the trust gate — but the exchange at that core is
	// doomed. The resolver must pre-compute the actionable hint that the
	// eventual exchange failure surfaces.
	const tokenCore = "https://core.eu.entire.io"
	const jurisdictionCore = "https://core.us.entire.io"
	srv, clusterHost := wellKnownServer(t, []string{tokenCore, jurisdictionCore}, "https://us.entire.io", jurisdictionCore)

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, tokenCore), clusterHost, t.TempDir(), srv.Client(),
	)
	if err != nil {
		t.Fatalf("cross-jurisdiction token must still resolve (it fails at exchange time), got: %v", err)
	}
	for _, want := range []string{tokenCore, clusterHost, "point your CI auth url at " + jurisdictionCore} {
		if !strings.Contains(creds.exchangeHint, want) {
			t.Errorf("exchangeHint missing %q, got %q", want, creds.exchangeHint)
		}
	}
}

func TestCrossJurisdictionHint_SameCoreVariantsStaySilent(t *testing.T) {
	t.Parallel()
	// Trailing-slash and case differences are the same core — a hint there
	// would cry wolf on healthy configs. An unadvertised jurisdiction core
	// (pre-upgrade server) also stays silent: there is nothing to point at.
	for _, tc := range []struct{ name, coreURL, jurisdictionCoreURL string }{
		{"exact", "https://core.us.entire.io", "https://core.us.entire.io"},
		{"trailing slash", "https://core.us.entire.io/", "https://core.us.entire.io"},
		{"case", "https://CORE.us.entire.io", "https://core.us.entire.io"},
		{"unadvertised", "https://core.us.entire.io", ""},
	} {
		if hint := crossJurisdictionHint(tc.coreURL, "cluster.example.com", tc.jurisdictionCoreURL); hint != "" {
			t.Errorf("%s: expected no hint, got %q", tc.name, hint)
		}
	}
}

func TestResolveEnvTokenCreds_MissingJurisdictionAudienceAborts(t *testing.T) {
	t.Parallel()
	// The cluster's core set trusts the token, but the cluster predates
	// jurisdiction-token git auth (no jurisdiction_audience). With no
	// repo-scoped fallback left, this must fail closed with the upgrade hint.
	const core = "https://core.us.entire.io"
	srv, clusterHost := wellKnownServer(t, []string{core}, "", "")

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, core), clusterHost, t.TempDir(), srv.Client(),
	)
	if err == nil {
		t.Fatal("expected missing jurisdiction_audience to be rejected")
	}
	if creds != nil {
		t.Fatal("expected nil creds when the cluster advertises no jurisdiction_audience")
	}
	if !strings.Contains(err.Error(), "jurisdiction_audience") {
		t.Fatalf("expected jurisdiction_audience error, got: %v", err)
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
		creds, err := resolveCreds(t.Context(), dummyURL, false, nil)
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

func TestResolveEnvTokenCreds_UntrustedAudAborts(t *testing.T) {
	t.Parallel()
	// The cluster advertises only core.us; the token's aud points elsewhere.
	// The gate must abort before building creds (i.e. before any exchange).
	srv, clusterHost := wellKnownServer(t, []string{"https://core.us.entire.io"}, "https://us.entire.io", "https://core.us.entire.io")

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://attacker.example.com"), clusterHost, t.TempDir(), srv.Client(),
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
	srv, clusterHost := wellKnownServer(t, []string{}, "https://us.entire.io", "https://core.us.entire.io")

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://core.us.entire.io"), clusterHost, t.TempDir(), srv.Client(),
	)
	if err == nil {
		t.Fatal("expected empty advertised core set to be rejected")
	}
	if creds != nil {
		t.Fatal("expected nil creds when no cores are advertised")
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

	creds, err := resolveEnvTokenCreds(
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
	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "http://core.us.entire.io"), "cluster.example.com", t.TempDir(), nil,
	)
	if err == nil {
		t.Fatal("expected http aud to be rejected before discovery")
	}
	if creds != nil {
		t.Fatal("expected nil creds for invalid aud")
	}
}
