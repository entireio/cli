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
		Email:     coreapi.NewOptString(testInviteEmail),
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
	assert.Contains(t, out, "INVITEE")
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
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, printJSON(w, &coreapi.ListOrgsOutputBody{Orgs: []coreapi.Org{{ID: testOrgULID, Name: "acme"}}}))
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
func TestOrgInvite_SendAndRevokeRequireATarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a missing --email or --id must be refused before any request")
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID)
	require.ErrorContains(t, err, "at least one of the flags in the group [email account] is required")
	_, _, err = runCoreCmd(t, newOrgCmd, srv.URL, "invite", "revoke", testOrgULID)
	require.ErrorContains(t, err, "at least one of the flags in the group [id email] is required")
}

// TestOrgInviteSend_AccountInvitesThroughTheOrgAdd: --account invites an
// existing account with the org add and the invitation allowed — the call
// `org grant add` makes — and reports the invitation as grant add does.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_AccountInvitesThroughTheOrgAdd(t *testing.T) {
	core, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusAccepted, invitedBody) })
	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--account", "github:alice", "--role", "admin")
	require.NoError(t, err)
	require.Equal(t, "✓ Invited github:alice to org "+testOrgULID+" as admin ("+inviteID+"). No access until they accept; the invitation expires 2026-11-20.\n", out)
	paths, bodies := core.recorded()
	require.Equal(t, []string{"/api/v1/orgs/" + testOrgULID + "/members"}, paths)
	require.Equal(t, true, bodies[0]["allow_invitation"])
	require.Equal(t, "admin", bodies[0]["role"])
	require.Equal(t, "uid-alice", bodies[0]["providerUserId"])
}

// TestOrgInviteSend_AccountJSON: --json is the add's own body, as for grant add.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_AccountJSON(t *testing.T) {
	_, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusAccepted, invitedBody) })
	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--account", "github:alice", "--json")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Equal(t, "invited", got["status"])
	require.Equal(t, inviteID, got["invitation_id"])
}

// TestOrgInviteSend_AccountOnACoreThatGrants: a core that does not require
// consent yet adds the account directly, and the command says so rather than
// calling a membership an invitation.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_AccountOnACoreThatGrants(t *testing.T) {
	_, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusCreated, membershipBody) })
	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--account", "github:alice")
	require.NoError(t, err)
	require.Equal(t, "✓ Granted github:alice member access to org "+testOrgULID+"\n", out)
}

// TestOrgInviteSend_AccountRefusals: Core's reason, and the revoke hint on a
// role mismatch.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_AccountRefusals(t *testing.T) {
	_, srv := newAddCore(t, func(string) addReply { return replyProblem(http.StatusConflict, "", "already a member") })
	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--account", "github:alice")
	require.EqualError(t, err, "already a member")

	_, srv = newAddCore(t, func(string) addReply {
		return replyProblem(http.StatusConflict, roleMismatchType, roleMismatchDetail)
	})
	_, _, err = runCoreCmd(t, newOrgCmd, srv.URL, "invite", "send", testOrgULID, "--account", "github:alice")
	require.EqualError(t, err, roleMismatchDetail+"\n"+inviteHint(cmdOrg, testOrgULID))
}

// TestOrgInviteSend_EmailAndAccountCannotBeConfused: each form is its own
// flag; giving both, neither, or a value that belongs to the other flag is
// refused before any request.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgInviteSend_EmailAndAccountCannotBeConfused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"both":                 {[]string{"--email", "dev@example.com", "--account", "github:alice"}, "none of the others can be"},
		"handle in --email":    {[]string{"--email", "github:alice"}, `invalid --email "github:alice": that is an account handle; invite it with --account github:alice`},
		"address in --account": {[]string{"--account", "dev@example.com"}, `invalid --account "dev@example.com": that is an email address; invite it with --email dev@example.com`},
		"ULID in --account":    {[]string{"--account", wiringGranteeULID}, "provider-qualified handle"},
		"bare name":            {[]string{"--account", "alice"}, "provider-qualified handle"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, append([]string{"invite", "send", testOrgULID}, tc.args...)...)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestOrgGrantAdd_HelpSaysItInvites: `org grant add` keeps its shape, and its
// help says what it does on Entire's API and names the verb for it.
func TestOrgGrantAdd_HelpSaysItInvites(t *testing.T) {
	t.Parallel()
	add, _, err := newOrgGrantCmd().Find([]string{"add"})
	require.NoError(t, err)
	require.Equal(t, "add <org> <grantee>", add.Use)
	require.Contains(t, add.Short, "Invite")
	require.Contains(t, add.Long, "invited rather than added")
	require.Contains(t, add.Long, "entire org invite send <org> --account <provider:handle>")
}
