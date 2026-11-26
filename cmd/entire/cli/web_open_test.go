package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stubWebOpenURL replaces the webOpenURL seam for the test and returns the
// URLs it was asked to open. openErr is what the stub answers. Callers must
// not be parallel: the seam is package-level.
func stubWebOpenURL(t *testing.T, openErr error) *[]string {
	t.Helper()
	var opened []string
	prev := webOpenURL
	webOpenURL = func(_ context.Context, url string) error {
		opened = append(opened, url)
		return openErr
	}
	t.Cleanup(func() { webOpenURL = prev })
	return &opened
}

// newWebOpenTestCmd is a minimal view command: --json, --web, and a RunE that
// opens target when --web is set.
func newWebOpenTestCmd(target string) (cmd *cobra.Command, stdout, stderr *bytes.Buffer) {
	cmd = &cobra.Command{
		Use:           "view",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if webRequested(cmd) {
				return openInWeb(cmd, target)
			}
			cmd.Println("printed")
			return nil
		},
	}
	addJSONFlag(cmd)
	addWebFlag(cmd)
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd, stdout, stderr
}

// Not parallel: sets ENTIRE_WEB_BASE_URL and swaps the webOpenURL seam.
func TestOpenInWeb_PrintsThenOpens(t *testing.T) {
	t.Setenv(api.WebBaseURLEnvVar, "https://web.example")
	for _, flag := range []string{"--web", "-w"} {
		t.Run(flag, func(t *testing.T) {
			opened := stubWebOpenURL(t, nil)
			cmd, stdout, stderr := newWebOpenTestCmd("https://web.example/gh/acme/web")
			cmd.SetArgs([]string{flag})
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			require.Equal(t, "https://web.example/gh/acme/web\n", stdout.String())
			require.Empty(t, stderr.String())
			require.Equal(t, []string{"https://web.example/gh/acme/web"}, *opened)
		})
	}
}

// Not parallel: sets ENTIRE_WEB_BASE_URL and swaps the webOpenURL seam.
func TestOpenInWeb_OpenerFailureIsANote(t *testing.T) {
	t.Setenv(api.WebBaseURLEnvVar, "https://web.example")
	opened := stubWebOpenURL(t, errors.New("no browser"))
	cmd, stdout, stderr := newWebOpenTestCmd("https://web.example/gh/acme/web")
	cmd.SetArgs([]string{"--web"})
	require.NoError(t, cmd.ExecuteContext(t.Context()), "the URL was delivered on stdout")
	require.Equal(t, "https://web.example/gh/acme/web\n", stdout.String())
	require.Contains(t, stderr.String(), "could not open a browser")
	require.Len(t, *opened, 1)
}

// Not parallel: swaps the package-level webOpenURL seam.
func TestOpenInWeb_EmptyURLErrors(t *testing.T) {
	opened := stubWebOpenURL(t, nil)
	cmd, stdout, _ := newWebOpenTestCmd("")
	cmd.SetArgs([]string{"--web"})
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "no web page is available")
	require.Empty(t, stdout.String())
	require.Empty(t, *opened)
}

// A server-supplied URL is checked before anything is printed or opened.
//
// Not parallel: sets ENTIRE_WEB_BASE_URL and swaps the webOpenURL seam.
func TestOpenInWeb_RefusesUnsafeURLs(t *testing.T) {
	t.Setenv(api.WebBaseURLEnvVar, "https://web.example:8443")
	for name, tc := range map[string]struct{ target, wantErr string }{
		"escape sequence":   {"https://web.example:8443/gh/a/b\x1b]0;owned\x07", "control characters"},
		"newline":           {"https://web.example:8443/gh/a/b\nrm -rf", "control characters"},
		"non-HTTP scheme":   {"file:///etc/passwd", "non-HTTP URL"},
		"javascript scheme": {"javascript:alert(1)", "non-HTTP URL"},
		"unparseable":       {"https://web.example:8443/%zz", "invalid URL"},
		"empty host":        {"https:///gh/a/b", "without a plain host"},
		"userinfo":          {"https://web.example:8443@evil.example/gh/a/b", "without a plain host"},
		"foreign host":      {"https://evil.example/gh/a/b", "not on the Entire web app origin https://web.example:8443"},
		"other port":        {"https://web.example/gh/a/b", "not on the Entire web app origin"},
		"other scheme":      {"http://web.example:8443/gh/a/b", "not on the Entire web app origin"},
	} {
		t.Run(name, func(t *testing.T) {
			opened := stubWebOpenURL(t, nil)
			cmd, stdout, stderr := newWebOpenTestCmd(tc.target)
			cmd.SetArgs([]string{"--web"})
			err := cmd.ExecuteContext(t.Context())
			require.ErrorContains(t, err, tc.wantErr)
			require.NotContains(t, err.Error(), "\x1b", "URL text in the error is sanitized")
			require.NotContains(t, err.Error(), "\n")
			require.Empty(t, stdout.String())
			require.Empty(t, stderr.String())
			require.Empty(t, *opened)
		})
	}

	t.Run("foreign host names the override", func(t *testing.T) {
		stubWebOpenURL(t, nil)
		cmd, _, _ := newWebOpenTestCmd("https://evil.example/gh/a/b")
		cmd.SetArgs([]string{"--web"})
		require.ErrorContains(t, cmd.ExecuteContext(t.Context()), api.WebBaseURLEnvVar)
	})
}

// Not parallel: sets ENTIRE_WEB_BASE_URL and swaps the webOpenURL seam.
func TestOpenInWeb_AcceptsTheWebOrigin(t *testing.T) {
	t.Setenv(api.WebBaseURLEnvVar, "https://Web.Example:8443/")
	opened := stubWebOpenURL(t, nil)
	// Host case differs from the origin's; the re-serialized form is what is
	// printed and opened.
	cmd, stdout, _ := newWebOpenTestCmd("HTTPS://WEB.example:8443/gh/acme/web/trails/7?tab=a b")
	cmd.SetArgs([]string{"--web"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	const want = "https://WEB.example:8443/gh/acme/web/trails/7?tab=a b"
	require.Equal(t, want+"\n", stdout.String())
	require.Equal(t, []string{want}, *opened)
}

// Not parallel: swaps the package-level webOpenURL seam.
func TestWebFlag_ExclusiveWithJSON(t *testing.T) {
	opened := stubWebOpenURL(t, nil)
	cmd, stdout, _ := newWebOpenTestCmd("https://web.example/gh/acme/web")
	cmd.SetArgs([]string{"--web", "--json"})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "none of the others can be")
	require.ErrorContains(t, err, "json web")
	require.Empty(t, stdout.String())
	require.Empty(t, *opened)
}

func TestWebFlag_WithoutJSONFlag(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "show"}
	addWebFlag(cmd)
	f := cmd.Flags().Lookup("web")
	require.NotNil(t, f)
	require.Equal(t, "w", f.Shorthand)
	require.False(t, webRequested(cmd))
	require.False(t, webRequested(&cobra.Command{Use: "plain"}), "an unregistered flag is not requested")
}

func TestRepoWebURL(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		base, forge, owner, repo, want string
	}{
		"github":              {"https://entire.com", "gh", "octocat", "hello-world", "https://entire.com/gh/octocat/hello-world"},
		"native":              {"https://entire.com", "et", "acme", "web", "https://entire.com/et/acme/web"},
		"trailing slash base": {"http://localhost:3000/", "et", "acme", "web", "http://localhost:3000/et/acme/web"},
		"escaped segments":    {"https://entire.com", "gh", "a/b", "r?x=1#f y", "https://entire.com/gh/a%2Fb/r%3Fx=1%23f%20y"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, repoWebURL(tc.base, tc.forge, tc.owner, tc.repo))
		})
	}
}

// runRepoViewWeb runs `repo view` against a control plane that fails the test
// when dialled: --web must build the URL without a core API call.
func runRepoViewWeb(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("repo view --web must not call the control plane, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return runCoreCmd(t, newRepoViewCmd, srv.URL, args...)
}

// Not parallel: sets ENTIRE_WEB_BASE_URL, changes CWD, and swaps the
// package-level webOpenURL and activeCoreClient seams.
func TestRepoView_Web(t *testing.T) {
	const base = "https://web.example"
	t.Setenv(api.WebBaseURLEnvVar, base+"/")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"github ref":       {[]string{"/gh/octocat/hello-world", "--web"}, base + "/gh/octocat/hello-world"},
		"native ref":       {[]string{"/et/acme/web", "-w"}, base + "/et/acme/web"},
		"entire clone URL": {[]string{"entire://aws-us-east-2.entire.io/et/acme/web.git", "--web"}, base + "/et/acme/web"},
	} {
		t.Run(name, func(t *testing.T) {
			opened := stubWebOpenURL(t, nil)
			stdout, stderr, err := runRepoViewWeb(t, tc.args...)
			require.NoError(t, err)
			require.Equal(t, tc.want+"\n", stdout)
			require.Empty(t, stderr)
			require.Equal(t, []string{tc.want}, *opened)
		})
	}

	for origin, want := range map[string]string{
		"https://github.com/octocat/hello-world.git":   base + "/gh/octocat/hello-world",
		"entire://aws-us-east-2.entire.io/et/acme/web": base + "/et/acme/web",
	} {
		t.Run("origin "+origin, func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.AddRemote(t, dir, "origin", origin)
			t.Chdir(dir)
			opened := stubWebOpenURL(t, nil)
			stdout, _, err := runRepoViewWeb(t, "--web")
			require.NoError(t, err)
			require.Equal(t, want+"\n", stdout)
			require.Equal(t, []string{want}, *opened)
		})
	}

	t.Run("authoritative is noted as ignored", func(t *testing.T) {
		opened := stubWebOpenURL(t, nil)
		stdout, stderr, err := runRepoViewWeb(t, "/et/acme/web", "--web", "--authoritative")
		require.NoError(t, err)
		require.Equal(t, base+"/et/acme/web\n", stdout)
		require.Contains(t, stderr, "--authoritative is ignored with --web")
		require.Len(t, *opened, 1)
	})

	t.Run("json is refused", func(t *testing.T) {
		opened := stubWebOpenURL(t, nil)
		// Cobra refuses the pair before RunE; stdout is its usage text here.
		stdout, _, err := runRepoViewWeb(t, "/et/acme/web", "--web", "--json")
		require.ErrorContains(t, err, "none of the others can be")
		require.NotContains(t, stdout, base)
		require.Empty(t, *opened)
	})

	t.Run("a bad ref is refused before anything opens", func(t *testing.T) {
		opened := stubWebOpenURL(t, nil)
		stdout, _, err := runRepoViewWeb(t, "octocat/hello-world", "--web")
		require.ErrorContains(t, err, "must name its forge")
		require.Empty(t, stdout)
		require.Empty(t, *opened)
	})
}
