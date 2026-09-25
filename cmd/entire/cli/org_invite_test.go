package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// withExtraProperty marshals v, then splices in one more top-level property
// ahead of its own fields. It lets a fixture keep using the modeled builders
// (testInvitation, etc.) to simulate an unmodeled server property instead of
// hand-typing every required field into a raw JSON literal.
func withExtraProperty(t *testing.T, v any, key, value string) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	s := string(raw)
	require.True(t, strings.HasPrefix(s, "{"), "fixture must marshal to a JSON object")
	return `{"` + key + `":"` + value + `",` + s[1:]
}

// testInvitationULID addresses an invitation directly, so the commands skip the
// by-email lookup and the fake only has to answer the revoke.
const testInvitationULID = "01HZX7QABCDEFGHJKMNPQRSTV3"

// testOrgULID is the <org> argument throughout: a ULID, so resolveOrgRef
// returns it without a by-name lookup.
const testOrgULID = "01HZX7QABCDEFGHJKMNPQRSTV4"

// testInviteEmail is the invited address every fixture carries.
const testInviteEmail = "dev@example.com"

func testInvitation(role, status string) *coreapi.Invitation {
	return &coreapi.Invitation{
		ID:        testInvitationULID,
		Email:     testInviteEmail,
		Role:      role,
		Status:    status,
		InvitedBy: testDeleteULID,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC),
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_CreatesAndReportsTheRole(t *testing.T) {
	var gotPath string
	var gotBody coreapi.CreateOrgInvitationInputBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, printJSON(w, testInvitation("admin", "open")))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--email", "dev@example.com", "--role", "admin")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/orgs/"+testOrgULID+"/invitations", gotPath)
	assert.Equal(t, "dev@example.com", gotBody.Email)
	assert.EqualValues(t, "admin", gotBody.Role)
	assert.Contains(t, out, "✓ Invited dev@example.com to org "+testOrgULID+" as admin ("+testInvitationULID+")")
}

// The wire field is required, so an omitted --role must still send the default
// rather than an empty string the server would reject.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_SendsTheDefaultRoleWhenFlagOmitted(t *testing.T) {
	var gotBody coreapi.CreateOrgInvitationInputBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, printJSON(w, testInvitation("member", "open")))
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--email", "dev@example.com")
	require.NoError(t, err)
	assert.EqualValues(t, "member", gotBody.Role)
}

// A 200 means an open invitation already existed and was resent with the role
// it was created with, which may not be the one just asked for.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_ResendReportsTheStoredRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, testInvitation("member", "open")))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--email", "dev@example.com", "--role", "admin")
	require.NoError(t, err)
	assert.Contains(t, out, "✓ Resent the open invitation for dev@example.com to org "+testOrgULID+", which invites as member ("+testInvitationULID+")")
	assert.NotContains(t, out, "as admin", "the request's role must not be reported as the effective one")
}

// Owner invites are the server's call, so a 403 is surfaced with the server's
// own explanation rather than pre-empted by a client-side role check.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_ForbiddenRoleSurfacesTheServerMessage(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, err := fmt.Fprint(w, `{"status":403,"detail":"only an owner may invite an owner"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--email", "dev@example.com", "--role", "owner")
	require.ErrorContains(t, err, "only an owner may invite an owner")
	assert.True(t, reached, "the CLI must ask the server rather than refuse an owner invite itself")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_RejectsAnUnknownRoleWithoutCallingTheServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an invalid --role must be refused before any request")
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--email", "dev@example.com", "--role", "auditor")
	require.ErrorContains(t, err, `invalid --role "auditor": must be one of owner, admin, member`)
}

// The generated response types round-trip any property the schema doesn't
// declare, so a server that ever sent one under an unmodeled key would
// otherwise reach --json output verbatim. An invitation is the one object with
// an accept token, so this pins that none can leak.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_JSONDropsUnmodeledResponseProperties(t *testing.T) {
	const leakedValue = "SHOULD-NEVER-REACH-JSON-OUTPUT"
	cases := []struct {
		name   string
		status int
	}{
		{name: "201 created", status: http.StatusCreated},
		{name: "200 resent", status: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				body := withExtraProperty(t, testInvitation("admin", "open"), "unexpectedField", leakedValue)
				_, err := fmt.Fprint(w, body)
				assert.NoError(t, err)
			}))
			t.Cleanup(srv.Close)

			out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--email", "dev@example.com", "--role", "admin", "--json")
			require.NoError(t, err)
			assert.NotContains(t, out, leakedValue, "an unmodeled response property reached --json output")
			assert.NotContains(t, out, "unexpectedField")
		})
	}
}

// The filter is the server's: the CLI passes --status through and renders what
// comes back, defaulting to the open invitations.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteList_ListsAndFiltersByStatus(t *testing.T) {
	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		invitations := []coreapi.Invitation{*testInvitation("admin", "open")}
		if gotStatus == "all" {
			invitations = append(invitations, *testInvitation("member", "revoked"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, &coreapi.ListOrgInvitationsOutputBody{Invitations: invitations}))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "list", testOrgULID)
	require.NoError(t, err)
	assert.Equal(t, "open", gotStatus, "the default listing is the open invitations")
	assert.Contains(t, out, "EMAIL")
	assert.Contains(t, out, testInvitationULID, "the ID is what `invite revoke` takes")
	assert.Contains(t, out, testInviteEmail)
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "open")
	assert.Contains(t, out, "2026-01-08", "the expiry is what a manager acts on")
	assert.NotContains(t, out, "revoked")

	out, _, err = runCoreCmd(t, newOrgCmd, srv.URL, "invite", "list", testOrgULID, "--status", "all")
	require.NoError(t, err)
	assert.Equal(t, "all", gotStatus)
	assert.Contains(t, out, "revoked", "a state the server returns is rendered, not filtered again")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteList_RejectsAnUnknownStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an invalid --status must be refused before any request")
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "list", testOrgULID, "--status", "pending")
	require.ErrorContains(t, err, `invalid --status "pending": must be one of open, accepted, revoked, expired, all`)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteList_ReportsAnEmptyListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, &coreapi.ListOrgInvitationsOutputBody{}))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "list", testOrgULID)
	require.NoError(t, err)
	assert.Contains(t, out, "No invitations found.")
}

// The list path loops over every invitation returned, so the create path's
// defense (see TestOrgInviteSend_JSONDropsUnmodeledResponseProperties) must hold
// for each item, not just the first — this is the case that matters more.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteList_JSONDropsUnmodeledResponsePropertiesOnEveryItem(t *testing.T) {
	const leaked1 = "SHOULD-NEVER-REACH-JSON-OUTPUT-1"
	const leaked2 = "SHOULD-NEVER-REACH-JSON-OUTPUT-2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := `{"invitations":[` +
			withExtraProperty(t, testInvitation("admin", "open"), "unexpectedField", leaked1) + "," +
			withExtraProperty(t, testInvitation("member", "open"), "unexpectedField", leaked2) +
			`]}`
		_, err := fmt.Fprint(w, body)
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "list", testOrgULID, "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, leaked1, "the first invitation's unmodeled property reached --json output")
	assert.NotContains(t, out, leaked2, "the second invitation's unmodeled property reached --json output")
	assert.NotContains(t, out, "unexpectedField")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteRevoke_ResolvesAnEmailThroughTheOpenListing(t *testing.T) {
	var deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		assert.Equal(t, "open", r.URL.Query().Get("status"), "an accepted or revoked invitation must not be matched")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, &coreapi.ListOrgInvitationsOutputBody{
			Invitations: []coreapi.Invitation{*testInvitation("member", "open")},
		}))
	}))
	t.Cleanup(srv.Close)

	// Mixed case: the server stores the address lowercased.
	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "revoke", testOrgULID, "--email", "Dev@Example.com")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/orgs/"+testOrgULID+"/invitations/"+testInvitationULID, deletedPath)
	assert.Contains(t, out, "✓ Revoked the invitation for Dev@Example.com")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteRevoke_IsANoOpWithoutAnOpenInvitation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Error("nothing is open for that address, so nothing may be revoked")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, &coreapi.ListOrgInvitationsOutputBody{}))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "revoke", testOrgULID, "--email", "gone@example.com")
	require.NoError(t, err)
	assert.Contains(t, out, "no open invitation; nothing to revoke")
}

// An invitation that disappears between the lookup and the revoke is the end
// state the user asked for, matching the other revoke verbs.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteRevoke_IsIdempotentOnAMissingInvitation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			writeNotFoundProblem(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, &coreapi.ListOrgInvitationsOutputBody{
			Invitations: []coreapi.Invitation{*testInvitation("member", "open")},
		}))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "revoke", testOrgULID, "--email", testInviteEmail)
	require.NoError(t, err)
	assert.Contains(t, out, "no such grant; nothing to revoke")
}

// The invite commands document the org by name only, so an unknown name must
// not suggest a ULID the way the shared org lookup does.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInvite_UnknownOrgNameHintsAtNamesOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, err := fmt.Fprint(w, `{"status":404,"detail":"org not found"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	for _, args := range [][]string{
		{"invite", "send", "ior", "--email", "dev@example.com"},
		{"invite", "list", "ior"},
		{"invite", "revoke", "ior", "--email", "dev@example.com"},
	} {
		_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, args...)
		require.EqualError(t, err, "no org named \"ior\" (run `entire org list` to see org names)", "%v", args)
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInvite_SendAndRevokeRequireEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a missing --email must be refused before any request")
	}))
	t.Cleanup(srv.Close)

	for _, verb := range []string{"send", "revoke"} {
		_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", verb, testOrgULID)
		require.ErrorContains(t, err, `required flag(s) "email" not set`, verb)
	}
}
