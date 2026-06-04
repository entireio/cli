package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

// wellKnownServer serves /.well-known/entire-cluster.json advertising the given
// cores over TLS, returning the server and the host:port to use as clusterHost.
func wellKnownServer(t *testing.T, cores []string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/entire-cluster.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"core_urls": cores}) //nolint:errcheck // best-effort in test stub
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
	srv, clusterHost := wellKnownServer(t, []string{core})

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, core), clusterHost,
		"https://cluster.example.com", t.TempDir(), srv.Client(),
	)
	if err != nil {
		t.Fatalf("expected trusted aud to succeed, got: %v", err)
	}
	if creds == nil {
		t.Fatal("expected non-nil creds for trusted aud")
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
		creds, err := resolveCreds(t.Context(), dummyURL, "https://cluster.example.com", false, nil)
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
	srv, clusterHost := wellKnownServer(t, []string{"https://core.us.entire.io"})

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://attacker.example.com"), clusterHost,
		"https://cluster.example.com", t.TempDir(), srv.Client(),
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
	srv, clusterHost := wellKnownServer(t, []string{})

	creds, err := resolveEnvTokenCreds(
		t.Context(), makeTestJWT(t, "https://core.us.entire.io"), clusterHost,
		"https://cluster.example.com", t.TempDir(), srv.Client(),
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
		t.Context(), makeTestJWT(t, "https://core.us.entire.io"), u.Host,
		"https://cluster.example.com", t.TempDir(), srv.Client(),
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
		t.Context(), makeTestJWT(t, "http://core.us.entire.io"), "cluster.example.com",
		"https://cluster.example.com", t.TempDir(), nil,
	)
	if err == nil {
		t.Fatal("expected http aud to be rejected before discovery")
	}
	if creds != nil {
		t.Fatal("expected nil creds for invalid aud")
	}
}
