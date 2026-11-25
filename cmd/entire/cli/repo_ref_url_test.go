package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// urlRefCore is a core that resolves acme/web to testDeleteULID and answers the
// verbs the URL-ref tests drive. It counts every request it sees.
func urlRefCore(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		repoPath := "/api/v1/repos/" + testDeleteULID
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/resolve":
			writeJSONResponse(t, w, http.StatusOK, nativeResolution(testDeletePath, testDeleteULID))
		case r.Method == http.MethodGet && r.URL.Path == repoPath+"/visibility":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"visibility":"private"}`)
		case r.Method == http.MethodGet && r.URL.Path == repoPath+"/branch-protection":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"rules":[{"pattern":"main","ref":"refs/heads/main"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == repoPath:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoRefCommands_AcceptEntireURLs(t *testing.T) {
	const url = "https://entire.com/et/acme/web"
	const path = "/et/acme/web"

	t.Run("visibility --json echoes the canonical path", func(t *testing.T) {
		srv, _ := urlRefCore(t)
		stdout, _, err := runCoreCmd(t, newRepoVisibilityGetCmd, srv.URL, url, "--json")
		require.NoError(t, err)
		require.Contains(t, stdout, path)
		require.NotContains(t, stdout, "entire.com")
	})

	t.Run("protection list resolves the URL like the path", func(t *testing.T) {
		srv, _ := urlRefCore(t)
		_, _, errURL := runCoreCmd(t, newRepoProtectionListCmd, srv.URL, url, "--json")
		_, _, errPath := runCoreCmd(t, newRepoProtectionListCmd, srv.URL, path, "--json")
		require.NoError(t, errURL)
		require.NoError(t, errPath)
	})

	t.Run("delete resolves the URL and names the canonical path", func(t *testing.T) {
		srv, _ := urlRefCore(t)
		stdout, _, err := runCoreCmd(t, newRepoDeleteCmd, srv.URL, url, "--force")
		require.NoError(t, err)
		require.Contains(t, stdout, "Deleted")
		require.NotContains(t, stdout, "entire.com")
	})

	t.Run("--project agreeing with the URL succeeds", func(t *testing.T) {
		srv, _ := urlRefCore(t)
		_, _, err := runCoreCmd(t, newRepoVisibilityGetCmd, srv.URL, url, "--project", "acme")
		require.NoError(t, err)
	})

	t.Run("--project disagreeing with the URL is a mismatch, echoing the typed ref", func(t *testing.T) {
		srv, _ := urlRefCore(t)
		_, _, err := runCoreCmd(t, newRepoVisibilityGetCmd, srv.URL, url, "--project", "other")
		require.EqualError(t, err, `supplied --project "other" does not match the project "acme" named in "`+url+`"`)
	})

	t.Run("bare host fails locally with no request", func(t *testing.T) {
		srv, calls := urlRefCore(t)
		for _, newCmd := range []func() *cobra.Command{newRepoVisibilityGetCmd, newRepoProtectionListCmd} {
			_, _, err := runCoreCmd(t, newCmd, srv.URL, "https://entire.com/")
			require.Error(t, err)
			require.Contains(t, err.Error(), "https://entire.com/")
		}
		_, _, err := runCoreCmd(t, newRepoDeleteCmd, srv.URL, "https://entire.com/", "--force")
		require.Error(t, err)
		require.Zero(t, calls.Load(), "a host-only ref must not reach the server")
	})
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoDefaultBranch_AcceptsEntireURLs(t *testing.T) {
	for _, ref := range []string{"/et/acme/web", "https://entire.com/et/acme/web", "entire.com/et/acme/web"} {
		t.Run(ref, func(t *testing.T) {
			inner := &fakeDefaultBranchServer{previous: "refs/heads/main"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/resolve" {
					writeJSONResponse(t, w, http.StatusOK, nativeResolution(testDeletePath, testDefaultBranchRepoULID))
					return
				}
				inner.handler(t)(w, r)
			}))
			t.Cleanup(srv.Close)
			_, _, err := runCoreCmd(t, newRepoDefaultBranchCmd, srv.URL, ref, "dev")
			require.NoError(t, err)
			require.Len(t, inner.bodies, 1, "the PUT reached the repo the path form resolves to")
			require.Equal(t, "dev", inner.bodies[0]["branch"])
		})
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoView_AcceptsEntireURLs(t *testing.T) {
	body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","visibility":"private"}`,
		testDeleteULID, testProjectULID)

	t.Run("native URL views the same repo as the path", func(t *testing.T) {
		srv, _ := serveRepoView(t, body, nil)
		want, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, "/et/acme/web", "--json")
		require.NoError(t, err)
		for _, ref := range []string{"https://entire.com/et/acme/web", "entire.com/et/acme/web/"} {
			got, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, ref, "--json")
			require.NoError(t, err, ref)
			require.Equal(t, want, got, ref)
		}
	})

	t.Run("a bad URL echoes the ref as typed", func(t *testing.T) {
		_, _, err := runCoreCmd(t, newRepoViewCmd, "http://unused.invalid", "https://entire.com/et/acme")
		require.Error(t, err)
		require.Contains(t, err.Error(), "https://entire.com/et/acme")
	})

	t.Run("credentials are redacted from the echoed ref", func(t *testing.T) {
		_, _, err := runCoreCmd(t, newRepoViewCmd, "http://unused.invalid", "https://u:secret@entire.com/et/acme/web")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	})
}
