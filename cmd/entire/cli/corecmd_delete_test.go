package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// testDeleteULID is a syntactically valid ULID (26 Crockford base32 chars, no
// I/L/O/U) so it passes looksLikeULID and the delete commands skip the name
// lookup, addressing the resource by id directly.
const testDeleteULID = "01HZX7QABCDEFGHJKMNPQRSTVW"

// writeNotFoundProblem writes a control-plane RFC 7807 404 so the ogen client
// decodes it as *ErrorModelStatusCode (which isNotFound keys on). A bare
// WriteHeader without the problem+json content type would instead surface as a
// decode error.
func writeNotFoundProblem(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := fmt.Fprintf(w, `{"status":%d,"detail":"not found"}`, http.StatusNotFound); err != nil {
		t.Errorf("write problem: %v", err)
	}
}

// runCoreCmd runs any active-context control-plane command against a seamed
// httptest core: it points the active-context client at srv via the
// activeCoreClient seam, runs newCmd() with args, and returns its stdout,
// stderr, and error. Commands dialing via runCoreForCluster (mirror
// create/remove/collaborators) bypass the seam and need their own httptest
// wiring. The caller must not be parallel: the seam is package-global.
//
// Note: cobra's cmd.Print* falls back to OutOrStderr(), which under SetOut
// resolves to the stdout buffer — so Empty(errOut) assertions in these tests
// only guard explicit ErrOrStderr writes; the Contains-on-stdout assertions
// are what pin the production stream.
func runCoreCmd(t *testing.T, newCmd func() *cobra.Command, srvURL string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srvURL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	cmd := newCmd()
	var out, errW bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errW)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errW.String(), err
}

// TestControlPlaneDelete_Wiring exercises the org/project/repo delete commands
// end-to-end through cobra: that --force bypasses the prompt and issues DELETE
// against the right path, that an already-gone resource (404) is idempotent,
// and that a non-interactive run without --force refuses rather than deleting
// unprompted.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestControlPlaneDelete_Wiring(t *testing.T) {
	cases := []struct {
		noun     string
		newCmd   func() *cobra.Command
		wantPath string
	}{
		{"org", newOrgDeleteCmd, "/api/v1/orgs/" + testDeleteULID},
		{"project", newProjectDeleteCmd, "/api/v1/projects/" + testDeleteULID},
		{"repo", newRepoDeleteCmd, "/api/v1/repos/" + testDeleteULID},
	}

	for _, tc := range cases {
		t.Run(tc.noun+"/force deletes via the right path", func(t *testing.T) {
			var gotMethod, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)

			out, errOut, err := runCoreCmd(t, tc.newCmd, srv.URL, testDeleteULID, "--force")
			require.NoError(t, err)
			require.Equal(t, http.MethodDelete, gotMethod)
			require.Equal(t, tc.wantPath, gotPath)
			require.Contains(t, out, "✓ Deleted "+tc.noun+" "+testDeleteULID)
			require.Empty(t, errOut, "no explicit ErrOrStderr writes expected")
		})

		t.Run(tc.noun+"/already-gone is idempotent", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeNotFoundProblem(t, w)
			}))
			t.Cleanup(srv.Close)

			out, errOut, err := runCoreCmd(t, tc.newCmd, srv.URL, testDeleteULID, "--force")
			require.NoError(t, err)
			require.Contains(t, out, "not found; nothing to delete")
			require.Empty(t, errOut)
		})

		t.Run(tc.noun+"/refuses without --force when non-interactive", func(t *testing.T) {
			// A ULID needs no resolve, so the refusal lands before any request.
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}))
			t.Cleanup(srv.Close)

			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, testDeleteULID)
			require.Error(t, err)
			require.Contains(t, err.Error(), "--force")
		})
	}
}
