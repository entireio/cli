package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestRepoCommands_TakePathsOnly pins the grammar every repo verb that acts on
// one repository accepts: its /et/<project>/<repo> path. A bare name, a ULID
// and a forge-less pair are each refused before any request, and there is no
// --project to scope a bare name with.
//
// Not parallel: swaps the package-level activeCoreClient seam via runCoreCmd.
func TestRepoCommands_TakePathsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name   string
		newCmd func() *cobra.Command
		// args follow the repo ref.
		args []string
		// mirror is true for verbs that answer a /gh/ ref themselves rather
		// than refusing it as a non-path.
		mirror bool
	}{
		{name: "repo delete", newCmd: newRepoDeleteCmd, args: []string{"--force"}},
		{name: "repo edit", newCmd: newRepoEditCmd, args: []string{"--visibility", "private"}},
		{name: "repo visibility get", newCmd: newRepoVisibilityGetCmd},
		{name: "repo protection list", newCmd: newRepoProtectionListCmd, mirror: true},
		{name: "repo protection add", newCmd: newRepoProtectionAddCmd, args: []string{"main"}, mirror: true},
		{name: "repo protection remove", newCmd: newRepoProtectionRemoveCmd, args: []string{"main"}, mirror: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Nil(t, tc.newCmd().Flags().Lookup(projectFlagName),
				"--project scoped a bare name, which this verb no longer takes")

			refs := []string{"web", "0123456789ABCDEFGHJKMNPQR5", "acme/web"}
			if !tc.mirror {
				refs = append(refs, "/gh/acme/web")
			}
			for _, ref := range refs {
				_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, append([]string{ref}, tc.args...)...)
				require.EqualErrorf(t, err, `repo "`+ref+`" must be a /et/<project>/<repo> path`, "ref %q", ref)
			}
		})
	}
}
