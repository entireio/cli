package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
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

// makeTestJWT builds a three-segment JWT (alg:HS256 so ParseClaims accepts it)
// carrying the given aud. The signature segment is filler — the env-token path
// reads the aud unverified and gates it on that core's cluster registry, never
// on the signature.
func makeTestJWT(t *testing.T, aud string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(fmt.Sprintf(`{"sub":"ci-runner","aud":%q}`, aud)))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

// fakeRegistry stands in for the core's GET /api/v1/clusters. It records the
// core it was built for so tests can assert which core the credential decision
// consulted — the whole point of the change is that it is the acting identity's
// core, never the target host itself.
type fakeRegistry struct {
	hosts   []string
	err     error
	coreURL string
	calls   int
}

func (f *fakeRegistry) ListClusters(context.Context) (*coreapi.ListClustersOutputBody, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	clusters := make([]coreapi.Cluster, 0, len(f.hosts))
	for _, h := range f.hosts {
		clusters = append(clusters, coreapi.Cluster{
			PublicUrl:    "https://" + h,
			ApiUrl:       coreapi.NewOptString("https://api." + h),
			Jurisdiction: "us",
			Slug:         h,
		})
	}
	return &coreapi.ListClustersOutputBody{Clusters: clusters}, nil
}

// registryFactory returns a clusterRegistryFactory serving reg, capturing the
// core URL it is asked to dial.
func registryFactory(reg *fakeRegistry) clusterRegistryFactory {
	return func(coreURL string, _ credentialProvider, _ bool) (coreapi.ClusterLister, error) {
		reg.coreURL = coreURL
		return reg, nil
	}
}

func TestResolveEnvTokenCreds_RegisteredClusterSucceeds(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	const clusterHost = "aws-us-east-2.entire.io"
	envToken := makeTestJWT(t, core)
	reg := &fakeRegistry{hosts: []string{"other.entire.io", clusterHost}}

	creds, _, err := resolveEnvTokenCreds(t.Context(), envToken, clusterHost, false, t.TempDir(), registryFactory(reg))
	if err != nil {
		t.Fatalf("expected a registered cluster to resolve, got: %v", err)
	}
	if reg.coreURL != core {
		t.Errorf("registry dialed %q, want the env token's aud core %q", reg.coreURL, core)
	}
	got, err := creds(t.Context())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got != envToken {
		t.Errorf("creds = %q, want ENTIRE_TOKEN verbatim", got)
	}
}

// A second git operation against an already-confirmed cluster must not pay
// another control-plane round trip: every clone/fetch/push resolves credentials,
// and the path this replaced served a warm on-disk cache.
func TestResolveEnvTokenCreds_WarmCacheSkipsTheRegistry(t *testing.T) {
	t.Parallel()
	const clusterHost = "aws-us-east-2.entire.io"
	envToken := makeTestJWT(t, "https://core.us.entire.io")
	reg := &fakeRegistry{hosts: []string{clusterHost}}
	cacheDir := t.TempDir()

	for i := range 3 {
		if _, _, err := resolveEnvTokenCreds(t.Context(), envToken, clusterHost, false, cacheDir, registryFactory(reg)); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
	if reg.calls != 1 {
		t.Fatalf("registry consulted %d times across 3 git ops, want 1", reg.calls)
	}
}

// A cluster host the token's core does not front must fail closed, with no
// credential built and no second opinion sought from the host itself.
func TestResolveEnvTokenCreds_UnregisteredClusterFails(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	envToken := makeTestJWT(t, core)
	reg := &fakeRegistry{hosts: []string{"aws-us-east-2.entire.io"}}

	creds, onUnauthorized, err := resolveEnvTokenCreds(t.Context(), envToken, "evil.example.com", false, t.TempDir(), registryFactory(reg))
	if err == nil {
		t.Fatal("an unregistered cluster host must fail")
	}
	if creds != nil || onUnauthorized != nil {
		t.Fatal("no credential may be built for an unregistered cluster host")
	}
	if !errors.Is(err, coreapi.ErrClusterNotRegistered) {
		t.Errorf("err = %v, want it to wrap ErrClusterNotRegistered", err)
	}
	for _, want := range []string{"evil.example.com", core, "entire auth use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err.Error(), want)
		}
	}
}

// A registry we cannot consult is a failure, never a fallback to the target
// host's own /.well-known claims.
func TestResolveEnvTokenCreds_RegistryUnavailableFails(t *testing.T) {
	t.Parallel()
	envToken := makeTestJWT(t, "https://core.us.entire.io")
	reg := &fakeRegistry{err: errors.New("connection refused")}

	creds, _, err := resolveEnvTokenCreds(t.Context(), envToken, "aws-us-east-2.entire.io", false, t.TempDir(), registryFactory(reg))
	if err == nil {
		t.Fatal("an unreachable cluster registry must fail the clone")
	}
	if creds != nil {
		t.Fatal("no credential may be built when the registry is unavailable")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %q, want the underlying registry failure", err.Error())
	}
}

// seedActiveContext writes a contexts.json with one active login on coreURL and
// points $ENTIRE_CONFIG_DIR at it. The keychain slot is populated because
// NewRefreshingLoginCredential requires one to build the token manager; no
// token is ever fetched — the registry fake never calls the provider.
func seedActiveContext(t *testing.T, coreURL string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	c := &contexts.Context{Name: "alice@us", CoreURL: coreURL, Handle: "alice", KeychainService: "entire-cli-test"}
	if err := contexts.Save(configDir, &contexts.File{CurrentContext: c.Name, Contexts: []*contexts.Context{c}}); err != nil {
		t.Fatalf("write contexts.json: %v", err)
	}
}

// The context path resolves the cluster against the ACTIVE context's core —
// not the target host's self-reported discovery document. Sets process-global
// env, so not parallel.
func TestResolveCreds_ContextPathVerifiesAgainstActiveCore(t *testing.T) {
	const core = "https://core.us.entire.io"
	const clusterHost = "aws-us-east-2.entire.io"
	seedActiveContext(t, core)
	t.Setenv(auth.EnvTokenVar, "")
	os.Unsetenv(auth.EnvTokenVar)

	reg := &fakeRegistry{hosts: []string{clusterHost}}
	creds, onUnauthorized, err := resolveCreds(
		t.Context(),
		&url.URL{Scheme: "entire", Host: clusterHost},
		false,
		&http.Client{},
		t.TempDir(),
		registryFactory(reg),
	)
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	if creds == nil || onUnauthorized == nil {
		t.Fatal("expected a credential provider for a registered cluster")
	}
	if reg.coreURL != core {
		t.Errorf("registry dialed %q, want the active context's core %q", reg.coreURL, core)
	}
	if reg.calls != 1 {
		t.Errorf("registry consulted %d times, want exactly 1", reg.calls)
	}
}

func TestResolveCreds_ContextPathUnregisteredClusterFails(t *testing.T) {
	const core = "https://core.us.entire.io"
	seedActiveContext(t, core)
	t.Setenv(auth.EnvTokenVar, "")
	os.Unsetenv(auth.EnvTokenVar)

	reg := &fakeRegistry{hosts: []string{"aws-us-east-2.entire.io"}}
	creds, _, err := resolveCreds(
		t.Context(),
		&url.URL{Scheme: "entire", Host: "evil.example.com"},
		false,
		&http.Client{},
		t.TempDir(),
		registryFactory(reg),
	)
	if err == nil {
		t.Fatal("an unregistered cluster host must fail")
	}
	if creds != nil {
		t.Fatal("no credential may be built for an unregistered cluster host")
	}
	if !errors.Is(err, coreapi.ErrClusterNotRegistered) {
		t.Errorf("err = %v, want it to wrap ErrClusterNotRegistered", err)
	}
}

func TestResolveCreds_ContextPathRegistryUnavailableFails(t *testing.T) {
	seedActiveContext(t, "https://core.us.entire.io")
	t.Setenv(auth.EnvTokenVar, "")
	os.Unsetenv(auth.EnvTokenVar)

	reg := &fakeRegistry{err: errors.New("connection refused")}
	creds, _, err := resolveCreds(
		t.Context(),
		&url.URL{Scheme: "entire", Host: "aws-us-east-2.entire.io"},
		false,
		&http.Client{},
		t.TempDir(),
		registryFactory(reg),
	)
	if err == nil {
		t.Fatal("an unreachable cluster registry must fail the clone")
	}
	if creds != nil {
		t.Fatal("no credential may be built when the registry is unavailable")
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
		creds, _, err := resolveCreds(t.Context(), dummyURL, false, nil, t.TempDir(), registryFactory(&fakeRegistry{}))
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

// An aud pointing at an attacker-chosen core buys nothing: that core's
// registry is then the one consulted, and it does not list the cluster the
// user typed, so the clone fails before any credential is attached.
func TestResolveEnvTokenCreds_AttackerAudAborts(t *testing.T) {
	t.Parallel()
	const clusterHost = "aws-us-east-2.entire.io"
	reg := &fakeRegistry{hosts: []string{"attacker-owned.example.com"}}

	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://attacker.example.com"), clusterHost, false, t.TempDir(), registryFactory(reg),
	)
	if err == nil {
		t.Fatal("expected an aud whose core does not front the cluster to be rejected")
	}
	if creds != nil {
		t.Fatal("expected nil creds when the token's core does not front the cluster")
	}
	if !errors.Is(err, coreapi.ErrClusterNotRegistered) {
		t.Fatalf("err = %v, want it to wrap ErrClusterNotRegistered", err)
	}
}

func TestResolveEnvTokenCreds_MalformedTokenAborts(t *testing.T) {
	t.Parallel()
	// A malformed aud must fail at the parse/validate step, before the registry
	// is consulted at all — so a nil factory is safe here.
	creds, _, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "http://core.us.entire.io"), "cluster.example.com", false, t.TempDir(), nil,
	)
	if err == nil {
		t.Fatal("expected http aud to be rejected before the registry lookup")
	}
	if creds != nil {
		t.Fatal("expected nil creds for invalid aud")
	}
}
