package cli

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSABind(t *testing.T) {
	route := "POST " + saBase + "/" + saTestID + "/bindings"

	t.Run("binds and prints the exchange inputs", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "bind", "acme/ci", "--github-repository", "acme/web", "--attribute", "ref=refs/heads/main")
		require.NoError(t, err)
		require.JSONEq(t, `{"providerId":"`+saTestProviderID+`","attributeFilter":{"repository":"acme/web","ref":"refs/heads/main"}}`, f.body(route))
		require.Contains(t, out, "✓ Bound acme/ci ("+saTestID+") to GitHub repository acme/web ("+saTestBindingID+")")
		require.Contains(t, out, "audience  "+srv.URL+"/api/authz/sts")
		require.Contains(t, out, "endpoint  "+srv.URL+"/oauth/token")
		require.Contains(t, out, "resource  "+saTestID)
	})

	t.Run("core's filter refusal passes through", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		detail := `invalid attribute filter: attribute "branch" is not one this provider maps`
		f.fail(route, http.StatusBadRequest, detail, "")
		_, err := runSA(t, srv.URL, "bind", "acme/ci", "--github-repository", "acme/web", "--attribute", "branch=main")
		require.EqualError(t, err, detail)
	})

	t.Run("an existing binding names the fix", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail(route, http.StatusConflict, "binding already exists for this provider and account", "")
		_, err := runSA(t, srv.URL, "bind", saTestID, "--github-repository", "acme/web")
		require.EqualError(t, err, saTestID+" already has a GitHub Actions binding; unbind it first")
	})

	t.Run("403 passes through", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail(route, http.StatusForbidden, "permission denied", "")
		_, err := runSA(t, srv.URL, "bind", saTestID, "--github-repository", "acme/web")
		require.EqualError(t, err, "permission denied")
	})

	t.Run("a glob sends nothing", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "bind", saTestID, "--github-repository", "acme/*")
		require.EqualError(t, err, `--github-repository "acme/*" must be one exact owner/repo`)
		require.Empty(t, f.callList())
	})

	t.Run("no GitHub provider", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.noGitHub = true
		_, err := runSA(t, srv.URL, "bind", saTestID, "--github-repository", "acme/web")
		require.EqualError(t, err, "this server has no GitHub Actions provider")
		require.NotContains(t, f.callList(), route)
	})

	t.Run("lists and removes bindings", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "bindings", saTestID)
		require.NoError(t, err)
		require.Contains(t, out, saTestBindingID)
		require.Contains(t, out, `{"repository":"acme/web"}`)

		out, err = runSA(t, srv.URL, "unbind", saTestID, saTestBindingID)
		require.NoError(t, err)
		require.Contains(t, out, "✓ Removed binding "+saTestBindingID+" from "+saTestID)
		require.Contains(t, f.callList(), "DELETE "+saBase+"/"+saTestID+"/bindings/"+saTestBindingID)
	})
}

func TestGitHubBindingFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		repo    string
		attrs   []string
		want    string
		wantErr string
	}{
		{repo: "acme/web", want: `{"repository":"acme/web"}`},
		{repo: "acme/web", attrs: []string{"ref=refs/heads/main", "event_name=push"}, want: `{"event_name":"push","ref":"refs/heads/main","repository":"acme/web"}`},
		{repo: "acme", wantErr: "must be one exact owner/repo"},
		{repo: "acme/web/x", wantErr: "must be one exact owner/repo"},
		{repo: "*/web", wantErr: "must be one exact owner/repo"},
		{repo: "acme/web", attrs: []string{"ref"}, wantErr: "must be name=value"},
		{repo: "acme/web", attrs: []string{"ref="}, wantErr: "must be name=value"},
		{repo: "acme/web", attrs: []string{"repository=acme/api"}, wantErr: "set the repository with --github-repository"},
		{repo: "acme/web", attrs: []string{"ref=a", "ref=b"}, wantErr: "--attribute ref is given twice"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s %v", tc.repo, tc.attrs), func(t *testing.T) {
			t.Parallel()
			got, err := githubBindingFilter(tc.repo, tc.attrs)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
		})
	}
}
