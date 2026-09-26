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
// DELETE — handle resolution for a provider:handle grantee, and the path
// lookup behind a /et/<project>/<repo> ref — and records the DELETE. record is
// called with the DELETE's method and path; deleteFn writes the DELETE
// response (e.g. 204 or a 404 problem).
func grantWiringHandler(t *testing.T, record func(method, path string), deleteFn func(w http.ResponseWriter)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !strings.HasSuffix(r.URL.Path, "/repos/resolve") {
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
		case strings.HasSuffix(r.URL.Path, "/repos/resolve"):
			payload = nativeResolution("acme/web", wiringRepoULID)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		if err := printJSON(w, payload); err != nil {
			t.Errorf("encode %s: %v", r.URL.Path, err)
		}
	}
}

// TestGrantRemove_RouteWiring drives the three `<noun> grant remove` commands
// through cobra and asserts that a grantee resolves and then hits the
// by-provider revoke route. The repo case also pins that a
// /et/<project>/<repo> ref resolves to the repo ULID the route needs.
//
// There is no by-ULID case because a grantee is a provider-qualified handle and
// nothing else — see TestGrantRemove_RefusesAULIDGrantee. The typed-id route
// still exists, but only the remove picker reaches it, with an id it read off a
// listing rather than one anybody typed.
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
			"project/by-provider",
			newProjectGrantCmd,
			[]string{"remove", wiringProjULID, "github:alice"},
			"/api/v1/projects/" + wiringProjULID + "/grants/account/github/12345",
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
// role contract: where --role has no server default (project and repo), leaving
// it out without a terminal to prompt on is refused locally, so the empty role
// never reaches validation, a lookup, or the API.
//
// --role is no longer a cobra-required flag, because cobra enforces those
// before RunE and a role that cannot reach RunE cannot be prompted for. The
// guarantee moved into the RunE, ahead of every request, which is what this
// test states; `go test` is non-interactive, so this is the refusing path.
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
			require.ErrorContains(t, err, "--role is required: one of reader, writer, admin")
		})
	}
}

// TestGrantRemove_RefusesAULIDGrantee pins that account ULIDs are not part of
// the interface. They were accepted once; a grantee is a provider-qualified
// handle now, and an id pasted out of a listing gets the same message as any
// other unqualified value rather than a route of its own.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_RefusesAULIDGrantee(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Errorf("revoked by ULID: %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	for name, tc := range map[string]struct {
		newCmd func() *cobra.Command
		ref    string
	}{
		"org":     {newOrgGrantCmd, wiringOrgULID},
		"project": {newProjectGrantCmd, wiringProjULID},
		"repo":    {newRepoGrantCmd, wiringRepoPath},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "remove", tc.ref, wiringGranteeULID)
			require.ErrorContains(t, err, "is an account ULID")
			require.ErrorContains(t, err, "provider-qualified handle")
		})
	}
}

// TestGrantRemove_HelpDoesNotOfferULIDs pins the other half: the form is gone
// from the help too, so nothing points a user at an argument that no longer
// works.
func TestGrantRemove_HelpDoesNotOfferULIDs(t *testing.T) {
	t.Parallel()
	for name, newCmd := range map[string]func() *cobra.Command{
		"org": newOrgGrantCmd, "project": newProjectGrantCmd, "repo": newRepoGrantCmd,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, c := range newCmd().Commands() {
				if strings.HasPrefix(c.Use, "remove") {
					// The TARGET is still addressable by ULID on org and
					// project; it is the GRANTEE that is a handle and nothing
					// else, so the check is on that clause.
					require.NotContains(t, c.Long, "account ULID")
					require.Contains(t, c.Long, "the grantee is a provider-qualified handle")
				}
			}
		})
	}
}

// TestGranteeErrorsNeverOfferAULID pins the wording as well as the rule. The
// split rule is shared with `project create --owner`, which does take a ULID
// and says so; reusing that message for a grantee offered a form this command
// refuses. ensureGranteeIsHandle borrows the rule and not the sentence.
func TestGranteeErrorsNeverOfferAULID(t *testing.T) {
	t.Parallel()
	for name, ref := range map[string]string{
		"unqualified":    "asdasdasd",
		"empty handle":   "github:",
		"empty provider": ":alice",
		"a ULID":         wiringGranteeULID,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ensureGranteeIsHandle(ref)
			require.Error(t, err)
			require.Contains(t, err.Error(), "grantee")
			require.Contains(t, err.Error(), "provider-qualified handle")
			require.NotContains(t, err.Error(), "ULID)", "no grantee error may offer the ULID form")
		})
	}

	// The owner ref is the caller that legitimately takes both, and keeps its
	// own wording.
	_, _, err := parseQualifiedHandle("asdasdasd")
	require.ErrorContains(t, err, "(or a ULID)")
	require.NoError(t, ensureGranteeIsHandle("github:alice"))
}
