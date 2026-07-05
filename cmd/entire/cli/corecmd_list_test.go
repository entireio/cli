package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// serveOrgList answers GET /api/v1/orgs with the given orgs, standing in for
// the control plane behind `entire org list`.
func serveOrgList(t *testing.T, orgs []coreapi.Org) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		if err := writeJSON(w, &coreapi.ListOrgsOutputBody{Orgs: orgs}); err != nil {
			t.Errorf("encode orgs: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRunCoreList_EmptyHumanMessageOnStdout(t *testing.T) {
	srv := serveOrgList(t, nil)
	out, errOut, err := runCoreCmd(t, newOrgListCmd, srv.URL)
	require.NoError(t, err)
	require.Contains(t, out, "No organizations found.")
	require.Empty(t, errOut, "empty-state message must go to stdout")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRunCoreList_EmptyJSONIsArray(t *testing.T) {
	srv := serveOrgList(t, nil)
	// org list's --json is persistent on the group root, so drive the full
	// group command with "list" as a subcommand arg.
	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "list", "--json")
	require.NoError(t, err)
	require.JSONEq(t, "[]", out, "empty --json list must be [], not null")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRunCoreList_RendersRows(t *testing.T) {
	srv := serveOrgList(t, []coreapi.Org{{ID: testDeleteULID, Name: "acme", Region: "us"}})
	out, errOut, err := runCoreCmd(t, newOrgListCmd, srv.URL)
	require.NoError(t, err)
	require.Contains(t, out, "NAME")
	require.Contains(t, out, "acme")
	require.Empty(t, errOut)
}
