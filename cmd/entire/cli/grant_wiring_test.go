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

// Valid ULID-shaped refs (26 Crockford base32 chars, no I/L/O/U) so the org and
// project remove commands short-circuit ref resolution and issue exactly the
// revoke call. A repo is addressed by its native path, which the handler below
// resolves to wiringRepoULID.
const (
	wiringRepoULID    = "01HZX7QABCDEFGHJKMNPQRSTVW"
	wiringProjULID    = "01HZX7QABCDEFGHJKMNPQRSTVX"
	wiringOrgULID     = "01HZX7QABCDEFGHJKMNPQRSTVY"
	wiringGranteeULID = "01HZX7QABCDEFGHJKMNPQRSTVZ"
	wiringRepoPath    = "/et/acme/web"
)

// grantWiringHandler serves the lookups a grant command makes before its
// DELETE — handle resolution for a provider:handle grantee, and the project and
// repo by-name lookups behind a /et/<project>/<repo> ref — and records the
// DELETE. record is called with the DELETE's method and path; deleteFn writes
// the DELETE response (e.g. 204 or a 404 problem).
func grantWiringHandler(t *testing.T, record func(method, path string), deleteFn func(w http.ResponseWriter)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			record(r.Method, r.URL.Path)
			deleteFn(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var payload any
		switch {
		case strings.Contains(r.URL.Path, "/identity/handles/"):
			payload = &coreapi.ResolvedIdentity{
				AccountId:      wiringGranteeULID,
				Provider:       providerGitHub,
				Handle:         "alice",
				ProviderUserId: "12345",
			}
		case strings.HasSuffix(r.URL.Path, "/repos"):
			payload = &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{ID: wiringRepoULID, Name: "web"})}
		case strings.HasSuffix(r.URL.Path, "/projects"):
			payload = &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{ID: wiringProjULID, Name: "acme", OwnerId: wiringOrgULID, OwnerType: coreapi.ProjectOwnerTypeOrg})}
		default:
			t.Errorf("unexpected GET %s", r.URL.Path)
			return
		}
		if err := printJSON(w, payload); err != nil {
			t.Errorf("encode %s: %v", r.URL.Path, err)
		}
	}
}

// TestGrantRemove_RouteWiring drives the three `<noun> grant remove` commands
// through cobra and asserts the grantee-form → route selection: a
// provider:handle grantee resolves then hits the by-provider revoke route,
// while an account ULID hits the typed-id route directly. The repo cases also
// pin that a /et/<project>/<repo> ref resolves to the repo ULID the route needs.
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
			newRepoGrantCmd,
			[]string{"remove", wiringRepoPath, "github:alice"},
			"/api/v1/repos/" + wiringRepoULID + "/grants/account/github/12345",
			"✓ Revoked github:alice from repo " + wiringRepoPath,
		},
		{
			"repo/by-grantee-id",
			newRepoGrantCmd,
			[]string{"remove", wiringRepoPath, wiringGranteeULID},
			"/api/v1/repos/" + wiringRepoULID + "/grants/account/" + wiringGranteeULID,
			"",
		},
		{
			"project/by-provider",
			newProjectGrantCmd,
			[]string{"remove", wiringProjULID, "github:alice"},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/github/12345",
			"",
		},
		{
			"project/by-grantee-id",
			newProjectGrantCmd,
			[]string{"remove", wiringProjULID, wiringGranteeULID},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/" + wiringGranteeULID,
			"",
		},
		{
			"org/by-provider",
			newOrgGrantCmd,
			[]string{"remove", wiringOrgULID, "github:alice"},
			"/api/v1/orgs/" + wiringOrgULID + "/members/github/12345",
			"✓ Revoked github:alice from org " + wiringOrgULID,
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
		{"repo/by-provider", newRepoGrantCmd, []string{"remove", wiringRepoPath, "github:alice"}},
		{"repo/by-grantee-id", newRepoGrantCmd, []string{"remove", wiringRepoPath, wiringGranteeULID}},
		{"project/by-provider", newProjectGrantCmd, []string{"remove", wiringProjULID, "github:alice"}},
		{"org/by-provider", newOrgGrantCmd, []string{"remove", wiringOrgULID, "github:alice"}},
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

// TestRepoGrant_TakesOnlyTheNativePath pins that `repo grant` addresses a repo
// by its /et/<project>/<repo> path and nothing else: a ULID and a bare name are
// refused before any request is made, and --project is not a flag here.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoGrant_TakesOnlyTheNativePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for _, ref := range []string{wiringRepoULID, "web"} {
		t.Run(ref, func(t *testing.T) {
			_, errOut, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", ref)
			require.Error(t, err)
			require.Contains(t, errOut+err.Error(), "/et/<project>/<repo>")
		})
	}

	t.Run("--project is not a flag", func(t *testing.T) {
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "add", wiringRepoPath, "github:alice", "--role", "reader", "--project", "acme")
		require.ErrorContains(t, err, "unknown flag: --project")
	})
}

// TestGrantAdd_EmptyRoleIsRefusedLocally pins that an explicit empty --role is
// an invalid role on every target, not a request: cobra's required-flag check
// only asks whether the flag was given, so `--role=` passes it with an empty
// value on project and repo, and on org (where --role is optional and omitting
// it means the server default) an explicit empty value is still a value the
// user typed. All three must fail role validation before any lookup or grant
// call is made.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_EmptyRoleIsRefusedLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for name, tc := range map[string]struct {
		newCmd func() *cobra.Command
		target string
	}{
		"org":     {newOrgGrantCmd, wiringOrgULID},
		"project": {newProjectGrantCmd, wiringProjULID},
		"repo":    {newRepoGrantCmd, wiringRepoPath},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "add", tc.target, "github:alice", "--role=")
			require.ErrorContains(t, err, `invalid --role ""`)
		})
	}
}

// TestGrantAdd_OmittedRequiredRoleIsRefusedLocally pins the other half of the
// role contract: where --role is required (project and repo), leaving it out
// fails cobra's required-flag check before RunE runs, so the empty role never
// reaches validation, a lookup, or the API. That check is what lets the RunE
// validate only a role the user typed.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_OmittedRequiredRoleIsRefusedLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for name, tc := range map[string]struct {
		newCmd func() *cobra.Command
		target string
	}{
		"project": {newProjectGrantCmd, wiringProjULID},
		"repo":    {newRepoGrantCmd, wiringRepoPath},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "add", tc.target, "github:alice")
			require.ErrorContains(t, err, `required flag(s) "role" not set`)
		})
	}
}
