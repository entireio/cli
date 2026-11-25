package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// recordingCore answers the verbs the per-site URL tests drive and records
// every request as "METHOD path?query body", so two spellings of one ref can
// be compared request for request.
func recordingCore(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/repos/resolve":
			assert.NoError(t, printJSON(w, nativeResolution(testDeletePath, testDeleteULID)))
		case strings.HasSuffix(r.URL.Path, "/visibility"):
			fmt.Fprint(w, `{"visibility":"private"}`)
		case strings.HasSuffix(r.URL.Path, "/branch-protection"):
			fmt.Fprint(w, `{"rules":[{"pattern":"main","ref":"refs/heads/main"}]}`)
		case r.URL.Path == "/api/v1/clusters":
			fmt.Fprint(w, `{"clusters":[]}`)
		default:
			// repo view of a GitHub upstream lists the repo index.
			assert.NoError(t, printJSON(w, &coreapi.ListReposOutputBody{
				Repos: []coreapi.RepoIndexEntry{{
					ID: "01R", Name: "r", FullName: "o/r",
					Org:      coreapi.NewOptString("o"),
					Provider: coreapi.NewOptString(repoProviderGitHub), Visibility: "public",
					Jurisdiction: "us", Cell: "c", ClusterSlug: "us",
				}},
			}))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reqs...)
	}
}

// TestRepoRefSites_URLSpellingsMatchThePath runs each command whose ref wiring
// has no other per-command URL test with the path form and both URL spellings,
// and requires identical output and identical outgoing requests. The ledger
// guard only pins that a file calls newTypedRef; this pins the behaviour of
// each command in it.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoRefSites_URLSpellingsMatchThePath(t *testing.T) {
	native := []string{"/et/acme/web", "https://entire.com/et/acme/web", "entire.com/et/acme/web"}
	github := []string{"/gh/o/r", "https://entire.com/gh/o/r", "entire.com/gh/o/r"}

	cases := []struct {
		name   string
		newCmd func() *cobra.Command
		refs   []string
		// args builds the argv for one spelling.
		args func(ref string) []string
	}{
		{"repo edit", newRepoEditCmd, native, func(ref string) []string { return []string{ref, "--visibility", "private", "--json"} }},
		{"repo protection add", newRepoProtectionAddCmd, native, func(ref string) []string { return []string{ref, "main", "--json"} }},
		{"repo protection remove", newRepoProtectionRemoveCmd, native, func(ref string) []string { return []string{ref, "main", "--json"} }},
		{"repo view /gh/", newRepoViewCmd, github, func(ref string) []string { return []string{ref, "--json"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wantOut string
			var wantReqs []string
			for i, ref := range tc.refs {
				srv, requests := recordingCore(t)
				out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args(ref)...)
				require.NoError(t, err, ref)
				require.NotContains(t, out, "entire.com", ref)
				if i == 0 {
					wantOut, wantReqs = out, requests()
					require.NotEmpty(t, wantReqs)
					continue
				}
				require.Equal(t, wantOut, out, ref)
				require.Equal(t, wantReqs, requests(), ref)
			}
		})
	}
}
