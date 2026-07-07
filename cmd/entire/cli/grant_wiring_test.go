package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// Valid ULID-shaped refs (26 Crockford base32 chars, no I/L/O/U) so the remove
// commands short-circuit ref resolution and issue exactly the revoke call.
const (
	wiringRepoULID    = "01HZX7QABCDEFGHJKMNPQRSTVW"
	wiringProjULID    = "01HZX7QABCDEFGHJKMNPQRSTVX"
	wiringOrgULID     = "01HZX7QABCDEFGHJKMNPQRSTVY"
	wiringGranteeULID = "01HZX7QABCDEFGHJKMNPQRSTVZ"
)

// grantWiringHandler serves the handle-resolution GET (so a provider:handle
// grantee resolves to a numeric provider user id) and records the subsequent
// revoke DELETE. record is called with the DELETE's method and path; deleteFn
// writes the DELETE response (e.g. 204 or a 404 problem).
func grantWiringHandler(t *testing.T, record func(method, path string), deleteFn func(w http.ResponseWriter)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/identity/handles/") {
			w.Header().Set("Content-Type", "application/json")
			if err := printJSON(w, &coreapi.ResolvedIdentity{
				AccountId:      wiringGranteeULID,
				Provider:       providerGitHub,
				Handle:         "alice",
				ProviderUserId: "12345",
			}); err != nil {
				t.Errorf("encode identity: %v", err)
			}
			return
		}
		record(r.Method, r.URL.Path)
		deleteFn(w)
	}
}

// TestGrantRemove_RouteWiring drives the grant remove commands through cobra and
// asserts the grantee-form → route selection: a provider:handle grantee resolves
// then hits the by-provider revoke route, while an account ULID hits the
// typed-id route directly. This locks in the grantee→route mapping.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_RouteWiring(t *testing.T) {
	cases := []struct {
		name       string
		newCmd     func() *cobra.Command
		args       []string
		wantPath   string
		wantOutput string // when set, the full success line; otherwise just "✓ " is checked
	}{
		{
			"repo/by-provider",
			newGrantRepoRemoveCmd,
			[]string{wiringRepoULID, "github:alice"},
			"/api/v1/repos/" + wiringRepoULID + "/grants/account/github/12345",
			"✓ Revoked github:alice from repo " + wiringRepoULID,
		},
		{
			"repo/by-grantee-id",
			newGrantRepoRemoveCmd,
			[]string{wiringRepoULID, wiringGranteeULID},
			"/api/v1/repos/" + wiringRepoULID + "/grants/account/" + wiringGranteeULID,
			"",
		},
		{
			"project/by-provider",
			newGrantProjectRemoveCmd,
			[]string{wiringProjULID, "github:alice"},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/github/12345",
			"",
		},
		{
			"project/by-grantee-id",
			newGrantProjectRemoveCmd,
			[]string{wiringProjULID, wiringGranteeULID},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/" + wiringGranteeULID,
			"",
		},
		{
			"org/by-provider",
			newGrantOrgRemoveCmd,
			[]string{wiringOrgULID, "github:alice"},
			"/api/v1/orgs/" + wiringOrgULID + "/members/github/12345",
			// org remove uses verb "Removed"; project/repo use "Revoked".
			"✓ Removed github:alice from org " + wiringOrgULID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			srv := httptest.NewServer(grantWiringHandler(t,
				func(method, path string) { gotMethod, gotPath = method, path },
				func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
			))
			t.Cleanup(srv.Close)

			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args...)
			require.NoError(t, err)
			require.Equal(t, http.MethodDelete, gotMethod)
			require.Equal(t, tc.wantPath, gotPath)
			require.Contains(t, out, "✓ ")
			if tc.wantOutput != "" {
				require.Contains(t, out, tc.wantOutput)
			}
		})
	}
}

// TestGrantRemove_Idempotent asserts that revoking an already-revoked grantee
// (the server answers 404) is a no-op success — "no such grant; nothing to
// revoke" — rather than surfacing a raw 404, matching the typed deletes.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_Idempotent(t *testing.T) {
	cases := []struct {
		name   string
		newCmd func() *cobra.Command
		args   []string
	}{
		{"repo/by-provider", newGrantRepoRemoveCmd, []string{wiringRepoULID, "github:alice"}},
		{"repo/by-grantee-id", newGrantRepoRemoveCmd, []string{wiringRepoULID, wiringGranteeULID}},
		{"project/by-provider", newGrantProjectRemoveCmd, []string{wiringProjULID, "github:alice"}},
		{"org/by-provider", newGrantOrgRemoveCmd, []string{wiringOrgULID, "github:alice"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(grantWiringHandler(t,
				func(_, _ string) {},
				func(w http.ResponseWriter) { writeNotFoundProblem(t, w) },
			))
			t.Cleanup(srv.Close)

			out, errOut, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args...)
			require.NoError(t, err)
			require.Contains(t, out, "nothing to revoke")
			require.Empty(t, errOut)
		})
	}
}
