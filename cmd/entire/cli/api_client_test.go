package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// The scheme gate has to run BEFORE credentials are resolved: discovery and the
// token refresh both dial the host, so checking afterwards would already have
// talked to it. NewAuthenticatedAPIClient relies on this one check rather than
// re-checking target.BaseURL afterwards, so this is what keeps that honest.
//
// Not parallel: it sets ENTIRE_API_BASE_URL and swaps the discovery seam.
func TestNewAuthenticatedAPIClient_RejectsInsecureOverrideBeforeResolving(t *testing.T) {
	unsetEnv(t, auth.EnvTokenVar)
	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Setenv(userdirs.EnvCacheHome, t.TempDir())
	t.Setenv(api.BaseURLEnvVar, "http://data.invalid")
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			t.Fatal("discovery ran against an insecure override")
			return nil, errors.New("unreachable")
		}))

	if _, err := NewAuthenticatedAPIClient(t.Context(), false); !errors.Is(err, api.ErrInsecureHTTP) {
		t.Fatalf("error = %v, want ErrInsecureHTTP", err)
	}
}

// ENTIRE_API_BASE_URL can carry userinfo, and this note reaches stderr and CI
// logs. Nothing from the userinfo, path or query may appear in it.
func TestInsecureDataOverrideNote_NeverEchoesCredentials(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"http://example.test", "ENTIRE_API_BASE_URL is set to an insecure http:// URL (http://example.test)"},
		{"http://example.test:8080/x?q=1", "ENTIRE_API_BASE_URL is set to an insecure http:// URL (http://example.test:8080)"},
		{"http://user:secret@example.test", "ENTIRE_API_BASE_URL is set to an insecure http:// URL (http://example.test)"},
		// Redacted() keeps the username, so a token in that slot would survive it.
		{"http://s3cr3t-token@example.test", "ENTIRE_API_BASE_URL is set to an insecure http:// URL (http://example.test)"},
		// No host to rebuild from: report the variable, not its value.
		{"http://user:secret@", "ENTIRE_API_BASE_URL is set to an insecure http:// URL"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(api.BaseURLEnvVar, tc.raw)
			got := insecureDataOverrideNote()
			if got != tc.want {
				t.Fatalf("note = %q, want %q", got, tc.want)
			}
			for _, leak := range []string{"secret", "s3cr3t-token", "q=1", "/x"} {
				if strings.Contains(got, leak) {
					t.Errorf("note leaks %q: %s", leak, got)
				}
			}
		})
	}
}

// --insecure-http-auth is the documented opt-in, so the same override must get
// through it — otherwise local dev against an http data host is unreachable.
func TestNewAuthenticatedAPIClient_InsecureFlagAllowsHTTPOverride(t *testing.T) {
	unsetEnv(t, auth.EnvTokenVar)
	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Setenv(userdirs.EnvCacheHome, t.TempDir())
	t.Setenv(api.BaseURLEnvVar, "http://data.invalid")
	discovered := false
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			discovered = true
			return nil, errors.New("no saved login")
		}))

	_, err := NewAuthenticatedAPIClient(t.Context(), true)
	if errors.Is(err, api.ErrInsecureHTTP) {
		t.Fatalf("error = %v, want the scheme gate bypassed under --insecure-http-auth", err)
	}
	if !discovered {
		t.Fatal("discovery did not run: the http override was rejected despite the opt-in")
	}
}
