package cli

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

const saBindRoute = "POST " + saBase + "/" + saTestID + "/bindings"

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSABind(t *testing.T) {
	t.Run("github-actions prints the workflow snippet", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "bind", "acme/ci", "--provider", "github-actions",
			"--pin", "repository=acme/web", "--pin", "ref=refs/heads/main")
		require.NoError(t, err)
		require.JSONEq(t, `{"providerId":"`+saTestProviderID+`","attributeFilter":{"repository":"acme/web","ref":"refs/heads/main"}}`, f.body(saBindRoute))
		require.Contains(t, out, "✓ Bound acme/ci ("+saTestID+") to GitHub Actions ("+saTestBindingID+")")
		require.Contains(t, out, "audience  "+srv.URL+"/api/authz/sts")
		require.Contains(t, out, "endpoint  "+srv.URL+"/oauth/token")
		require.Contains(t, out, "resource  "+saTestID)
		require.Contains(t, out, "docs      https://docs.github.com/oidc")
		require.Contains(t, out, "- uses: entireio/setup-entire@v1")
		require.Contains(t, out, "service-account: "+saTestID)
		require.Contains(t, out, "auth-url: "+srv.URL)
	})

	t.Run("buildkite gets the generic next step", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "buildkite",
			"--pin", "organization=acme", "--pin", "pipeline=deploy")
		require.NoError(t, err)
		require.JSONEq(t, `{"providerId":"`+saTestBuildkiteID+`","attributeFilter":{"organization":"acme","pipeline":"deploy"}}`, f.body(saBindRoute))
		require.Contains(t, out, "to Buildkite ("+saTestBindingID+")")
		require.Contains(t, out, "docs      https://buildkite.com/docs/agent/v3/cli-oidc")
		require.NotContains(t, out, "setup-entire")
	})

	refusals := []struct {
		name string
		args []string
		want string
	}{
		{"missing required pin", []string{"--provider", "buildkite", "--pin", "organization=acme"}, "buildkite needs --pin for: pipeline"},
		{"glob on an exact pin", []string{"--provider", "buildkite", "--pin", "organization=ac*", "--pin", "pipeline=deploy"}, "pin organization must be exact, not a glob"},
		{"pattern mismatch", []string{"--provider", "github-actions", "--pin", "repository=acme"}, `pin repository="acme" must match ^[^/*]+/[^/*]+$`},
		{"unknown pin", []string{"--provider", "buildkite", "--pin", "organization=acme", "--pin", "pipeline=deploy", "--pin", "repo=x"}, `buildkite has no pin "repo"; see entire sa providers buildkite`},
		{"unknown provider", []string{"--provider", "gitlab", "--pin", "project=acme"}, `no provider named "gitlab"; run entire sa providers`},
		{"operator-only provider", []string{"--provider", "cloudflare-access", "--pin", "common_name=x"}, "cloudflare-access is operator-only"},
	}
	for _, tc := range refusals {
		t.Run(tc.name+" sends no bind", func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			_, err := runSA(t, srv.URL, append([]string{"bind", saTestID}, tc.args...)...)
			require.EqualError(t, err, tc.want)
			require.NotContains(t, f.callList(), saBindRoute)
		})
	}

	t.Run("no pins for a provider with none required", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.providers = append(f.providers, coreapi.OIDCProvider{
			ID: "wip_optional", Issuer: "https://ci.example.com",
			Name: coreapi.NewOptString("optional-ci"), SelfServe: coreapi.NewOptBool(true),
			Attributes: []coreapi.OIDCProviderAttribute{{Name: "subject", Label: "Subject"}},
		})
		_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "optional-ci")
		require.EqualError(t, err, "pass at least one --pin name=value")
		require.NotContains(t, f.callList(), saBindRoute)
	})

	t.Run("a bad pin flag sends nothing", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "buildkite", "--pin", "organization")
		require.EqualError(t, err, `--pin "organization" must be name=value`)
		require.Empty(t, f.callList())
	})

	typed := []struct {
		name   string
		status int
		typ    string
		want   string
	}{
		{"binding exists", http.StatusConflict, saProblemBindingExists, saTestID + " already has a github-actions binding; unbind it first"},
		{"provider not self-serve", http.StatusForbidden, saProblemNotSelfServe, "github-actions is operator-only"},
		{"untyped 409 keeps core's prose", http.StatusConflict, "", "binding already exists for this provider and account"},
	}
	for _, tc := range typed {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			f.fail(saBindRoute, tc.status, "binding already exists for this provider and account", tc.typ)
			_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "github-actions", "--pin", "repository=acme/web")
			require.EqualError(t, err, tc.want)
		})
	}

	t.Run("core's filter refusal passes through", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		detail := "invalid attribute filter: pin repository; this provider requires it"
		f.fail(saBindRoute, http.StatusBadRequest, detail, "")
		_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "github-actions", "--pin", "repository=acme/web")
		require.EqualError(t, err, detail)
	})
}

// An old core sends displayName and nothing else.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestSABind_OldCore(t *testing.T) {
	t.Run("github-actions binds with no client checks", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.providers = saOldCoreProviders()
		out, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "github-actions", "--pin", "anything=x*")
		require.NoError(t, err)
		require.JSONEq(t, `{"providerId":"`+saTestProviderID+`","attributeFilter":{"anything":"x*"}}`, f.body(saBindRoute))
		require.Contains(t, out, "to github-actions (")
	})

	t.Run("another provider is left to core", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.providers = saOldCoreProviders()
		_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "cloudflare-access", "--pin", "common_name=x")
		require.NoError(t, err)
		require.Contains(t, f.callList(), saBindRoute)
	})

	t.Run("no pins at all", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.providers = saOldCoreProviders()
		_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "github-actions")
		require.EqualError(t, err, "pass at least one --pin name=value")
		require.NotContains(t, f.callList(), saBindRoute)
	})

	t.Run("lists both providers", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.providers = saOldCoreProviders()
		out, err := runSA(t, srv.URL, "providers")
		require.NoError(t, err)
		require.Contains(t, out, "github-actions")
		require.Contains(t, out, "cloudflare-access")
	})
}

// Not parallel: sets EnvTestTTY and swaps seams.
func TestSABind_PromptsForMissingPins(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	var asked []string
	prev := askPins
	askPins = func(_ *cobra.Command, _ saProvider, missing []coreapi.OIDCProviderAttribute) (map[string]string, error) {
		asked = attributeNames(missing)
		return map[string]string{"pipeline": "deploy"}, nil
	}
	t.Cleanup(func() { askPins = prev })

	f, srv := newSAFakeCore(t)
	_, err := runSA(t, srv.URL, "bind", saTestID, "--provider", "buildkite", "--pin", "organization=acme")
	require.NoError(t, err)
	require.Equal(t, []string{"pipeline"}, asked)
	require.JSONEq(t, `{"providerId":"`+saTestBuildkiteID+`","attributeFilter":{"organization":"acme","pipeline":"deploy"}}`, f.body(saBindRoute))
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSAProviders(t *testing.T) {
	t.Run("lists only self-serve providers", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "providers")
		require.NoError(t, err)
		require.Contains(t, out, "REQUIRED PINS")
		require.Contains(t, out, "GitHub Actions")
		require.Contains(t, out, "organization, pipeline")
		require.NotContains(t, out, "cloudflare-access")
	})

	t.Run("json keeps the wire records", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "providers", "--json")
		require.NoError(t, err)
		var got []map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		require.Len(t, got, 2)
		require.Equal(t, "github-actions", got[0]["name"])
		require.Equal(t, true, got[0]["self_serve"])
	})

	t.Run("one provider shows every attribute", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "providers", "github-actions")
		require.NoError(t, err)
		require.Contains(t, out, "GitHub Actions (github-actions)")
		require.Contains(t, out, "Docs: https://docs.github.com/oidc")
		require.Contains(t, out, "PATTERN")
		require.Contains(t, out, `^[^/*]+/[^/*]+$`)
		require.Contains(t, out, "ref")
	})

	t.Run("an operator-only provider is refused by name", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "providers", "cloudflare-access")
		require.EqualError(t, err, "cloudflare-access is operator-only")
	})

	t.Run("an old core describes no attributes", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.providers = saOldCoreProviders()
		out, err := runSA(t, srv.URL, "providers", "github-actions")
		require.NoError(t, err)
		require.Contains(t, out, "Core describes no attributes for this provider.")
	})
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSABindingsAndUnbind(t *testing.T) {
	f, srv := newSAFakeCore(t)
	out, err := runSA(t, srv.URL, "bindings", saTestID)
	require.NoError(t, err)
	require.Contains(t, out, saTestBindingID)
	require.Contains(t, out, "github-actions")
	require.NotContains(t, out, saTestProviderID)
	require.Contains(t, out, `{"repository":"acme/web"}`)

	out, err = runSA(t, srv.URL, "unbind", saTestID, saTestBindingID)
	require.NoError(t, err)
	require.Contains(t, out, "✓ Removed binding "+saTestBindingID+" from "+saTestID)
	require.Contains(t, f.callList(), "DELETE "+saBase+"/"+saTestID+"/bindings/"+saTestBindingID)
}

func TestSAProviderAccess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		p    coreapi.OIDCProvider
		want saProviderAccess
	}{
		{"self-serve", coreapi.OIDCProvider{Name: coreapi.NewOptString("buildkite"), SelfServe: coreapi.NewOptBool(true)}, saProviderSelfServe},
		{"operator-only", coreapi.OIDCProvider{Name: coreapi.NewOptString("eks"), SelfServe: coreapi.NewOptBool(false)}, saProviderOperatorOnly},
		{"old core github-actions", withDisplayName(coreapi.OIDCProvider{}, "github-actions"), saProviderSelfServe},
		{"old core other", withDisplayName(coreapi.OIDCProvider{}, "buildkite"), saProviderUnknown},
		{"false beats the github-actions name", coreapi.OIDCProvider{Name: coreapi.NewOptString("github-actions"), SelfServe: coreapi.NewOptBool(false)}, saProviderOperatorOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, newSAProvider(tc.p).access)
		})
	}
}

func TestParsePins(t *testing.T) {
	t.Parallel()

	got, err := parsePins([]string{" ref = refs/heads/main ", "event_name=push"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"ref": "refs/heads/main", "event_name": "push"}, got)

	for arg, want := range map[string]string{
		"ref":  `--pin "ref" must be name=value`,
		"ref=": `--pin "ref=" must be name=value`,
		"=x":   `--pin "=x" must be name=value`,
	} {
		_, err := parsePins([]string{arg})
		require.EqualError(t, err, want)
	}
	_, err = parsePins([]string{"ref=a", "ref=b"})
	require.EqualError(t, err, "--pin ref is given twice")
}
