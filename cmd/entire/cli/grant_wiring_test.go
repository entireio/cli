package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// Valid ULID-shaped refs (26 Crockford base32 chars, no I/L/O/U) so the remove
// commands short-circuit ref resolution and issue exactly the revoke call.
const (
	wiringRepoULID    = "01HZX7QABCDEFGHJKMNPQRSTVW"
	wiringProjULID    = "01HZX7QABCDEFGHJKMNPQRSTVX"
	wiringOrgULID     = "01HZX7QABCDEFGHJKMNPQRSTVY"
	wiringGranteeULID = "01HZX7QABCDEFGHJKMNPQRSTVZ"
)

// TestGrantRemove_RouteWiring drives the grant remove commands through cobra and
// asserts the grantee-mode → route selection: --provider/--provider-user-id must
// hit the by-provider revoke route, while --grantee-type/--grantee-id must hit
// the typed-id route. This locks in the mode→route mapping that grant_test.go's
// pure-helper tests (parseGranteeMode) can't observe.
//
// Not parallel: runDeleteCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_RouteWiring(t *testing.T) {
	cases := []struct {
		name     string
		newCmd   func() *cobra.Command
		args     []string
		wantPath string
	}{
		{
			"repo/by-provider",
			newGrantRepoRemoveCmd,
			[]string{wiringRepoULID, "--provider", "github", "--provider-user-id", "12345"},
			"/api/v1/repos/" + wiringRepoULID + "/grants/account/github/12345",
		},
		{
			"repo/by-grantee-id",
			newGrantRepoRemoveCmd,
			[]string{wiringRepoULID, "--grantee-type", "account", "--grantee-id", wiringGranteeULID},
			"/api/v1/repos/" + wiringRepoULID + "/grants/account/" + wiringGranteeULID,
		},
		{
			"project/by-provider",
			newGrantProjectRemoveCmd,
			[]string{wiringProjULID, "--provider", "github", "--provider-user-id", "12345"},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/github/12345",
		},
		{
			"project/by-grantee-id",
			newGrantProjectRemoveCmd,
			[]string{wiringProjULID, "--grantee-type", "account", "--grantee-id", wiringGranteeULID},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/" + wiringGranteeULID,
		},
		{
			"org/by-provider",
			newGrantOrgRemoveCmd,
			[]string{wiringOrgULID, "--provider", "github", "--provider-user-id", "12345"},
			"/api/v1/orgs/" + wiringOrgULID + "/members/github/12345",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)

			_, err := runDeleteCmd(t, tc.newCmd, srv.URL, tc.args...)
			require.NoError(t, err)
			require.Equal(t, http.MethodDelete, gotMethod)
			require.Equal(t, tc.wantPath, gotPath)
		})
	}
}

// TestGrantRemove_Idempotent asserts that revoking an already-revoked grantee
// (the server answers 404) is a no-op success — "no such grant; nothing to
// revoke" — rather than surfacing a raw 404, matching the typed deletes.
//
// Not parallel: runDeleteCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_Idempotent(t *testing.T) {
	cases := []struct {
		name   string
		newCmd func() *cobra.Command
		args   []string
	}{
		{"repo/by-provider", newGrantRepoRemoveCmd, []string{wiringRepoULID, "--provider", "github", "--provider-user-id", "12345"}},
		{"repo/by-grantee-id", newGrantRepoRemoveCmd, []string{wiringRepoULID, "--grantee-type", "account", "--grantee-id", wiringGranteeULID}},
		{"project/by-provider", newGrantProjectRemoveCmd, []string{wiringProjULID, "--provider", "github", "--provider-user-id", "12345"}},
		{"org/by-provider", newGrantOrgRemoveCmd, []string{wiringOrgULID, "--provider", "github", "--provider-user-id", "12345"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeNotFoundProblem(t, w)
			}))
			t.Cleanup(srv.Close)

			out, err := runDeleteCmd(t, tc.newCmd, srv.URL, tc.args...)
			require.NoError(t, err)
			require.Contains(t, out, "nothing to revoke")
		})
	}
}
