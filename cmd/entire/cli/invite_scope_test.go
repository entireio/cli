package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// `<noun> invite list` and `<noun> invite revoke` on the three scopes. The org
// verbs' email paths are covered in org_invite_test.go; these cover what every
// scope shares: the invitee column for account-targeted invitations, revoke
// by id, and the project and repo routes.

const (
	accountInviteULID = "01M7BDT64K8BTHRT2996R6XNG2"
	unresolvedInvite  = "01M7BDT64K8BTHRT2996R6XNG3"
	emailInviteULID   = "01M7BDT64K8BTHRT2996R6XNG4"
	inviteeAccount    = "01HZX7QABCDEFGHJKMNPQRSTA1"
	ghostAccount      = "01HZX7QABCDEFGHJKMNPQRSTA2"
)

// inviteScopeCase is one scope's command and the routes its verbs reach.
type inviteScopeCase struct {
	name     string
	newCmd   func() *cobra.Command
	ref      string // the scope as typed
	base     string // the scope's invitations route
	resource bool   // project or repo: ResourceInvitation; org: Invitation
}

func inviteScopeCases() []inviteScopeCase {
	return []inviteScopeCase{
		{"org", newOrgCmd, testOrgULID, "/api/v1/orgs/" + testOrgULID + "/invitations", false},
		{"project", newProjectCmd, wiringProjULID, "/api/v1/projects/" + wiringProjULID + "/invitations", true},
		{"repo", newRepoCmd, wiringRepoPath, "/api/v1/repos/" + wiringRepoULID + "/invitations", true},
	}
}

// invitationsFixture is one listing's raw JSON entries: an email invitation,
// an account invitation whose handle Core resolved, and one it could not.
func invitationsFixture(resource bool) string {
	role := "member"
	scope := ""
	if resource {
		role = "writer"
		scope = `"scope":"project","scopeId":"` + wiringProjULID + `",`
	}
	entry := func(id, invitee string) string {
		return `{"id":"` + id + `",` + scope + invitee + `,"role":"` + role + `","status":"open","invitedBy":"01HZX7QABCDEFGHJKMNPQRSTVZ","createdAt":"2026-11-13T21:42:56Z","expiresAt":"2026-11-20T21:42:56Z","unexpectedField":"SHOULD-NOT-REACH-JSON"}`
	}
	return `{"invitations":[` +
		entry(emailInviteULID, `"email":"dev@example.com"`) + "," +
		entry(accountInviteULID, `"email":"","inviteeAccountId":"`+inviteeAccount+`","inviteeIdentity":{"handles":[{"provider":"github","handle":"bob","isPrimary":false},{"provider":"github","handle":"alice","isPrimary":true}]}`) + "," +
		entry(unresolvedInvite, `"inviteeAccountId":"`+ghostAccount+`"`) +
		`]}`
}

// inviteCore answers a scope's invitations routes and the repo path lookup,
// and records each request after the lookup as "METHOD path?query".
type inviteCore struct {
	mu       sync.Mutex
	requests []string
}

func (c *inviteCore) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func newInviteCore(t *testing.T, list string, deleteStatus int) (*inviteCore, *httptest.Server) {
	t.Helper()
	core := &inviteCore{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/resolve") {
			writeJSONResponse(t, w, http.StatusOK, nativeResolution("acme/web", wiringRepoULID))
			return
		}
		core.mu.Lock()
		core.requests = append(core.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		core.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/invitations"):
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, list); err != nil {
				t.Errorf("write listing: %v", err)
			}
		case r.Method == http.MethodDelete && deleteStatus == http.StatusNotFound:
			writeNotFoundProblem(t, w)
		case r.Method == http.MethodDelete:
			w.WriteHeader(deleteStatus)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return core, srv
}

// TestInviteList_ShowsEveryKindOfInvitee: an email invitation shows its
// address, an account invitation the account's primary handle, and one whose
// handle Core could not resolve the account ID.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteList_ShowsEveryKindOfInvitee(t *testing.T) {
	for _, tc := range inviteScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			core, srv := newInviteCore(t, invitationsFixture(tc.resource), http.StatusNoContent)
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "list", tc.ref)
			require.NoError(t, err)
			require.Equal(t, []string{"GET " + tc.base + "?status=open"}, core.recorded())
			lines := strings.Split(strings.TrimSpace(out), "\n")
			require.Len(t, lines, 4, out)
			require.Equal(t, []string{"ID", "INVITEE", "ROLE", "STATUS", "EXPIRES"}, strings.Fields(lines[0]))
			require.Equal(t, emailInviteULID, strings.Fields(lines[1])[0])
			require.Equal(t, "dev@example.com", strings.Fields(lines[1])[1])
			require.Equal(t, []string{accountInviteULID, "github:alice"}, strings.Fields(lines[2])[:2], "the primary handle")
			require.Equal(t, []string{unresolvedInvite, ghostAccount}, strings.Fields(lines[3])[:2], "the account ID when no handle resolved")
			require.Contains(t, lines[2], "2026-11-20")
		})
	}
}

// TestInviteList_JSONPassesTheInvitationsThrough: --json is the listing
// itself, account fields included, without the properties the schema does not
// declare.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteList_JSONPassesTheInvitationsThrough(t *testing.T) {
	for _, tc := range inviteScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newInviteCore(t, invitationsFixture(tc.resource), http.StatusNoContent)
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "list", tc.ref, "--json")
			require.NoError(t, err)
			require.NotContains(t, out, "SHOULD-NOT-REACH-JSON")
			var got []map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			require.Len(t, got, 3)
			require.Equal(t, inviteeAccount, got[1]["inviteeAccountId"])
			require.Equal(t, accountInviteULID, got[1]["id"])
		})
	}
}

// TestInviteList_ScopeNotFound: the scope's 404 is Core's reason.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteList_ScopeNotFound(t *testing.T) {
	for _, tc := range inviteScopeCases()[1:] {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/repos/resolve") {
					writeJSONResponse(t, w, http.StatusOK, nativeResolution("acme/web", wiringRepoULID))
					return
				}
				writeCoreProblem(t, w, http.StatusNotFound, tc.name+" not found")
			}))
			t.Cleanup(srv.Close)
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "list", tc.ref)
			require.EqualError(t, err, tc.name+" not found")
		})
	}
}

// TestInviteRevoke_ByID revokes the invitation the id names, with no listing.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteRevoke_ByID(t *testing.T) {
	for _, tc := range inviteScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			core, srv := newInviteCore(t, `{"invitations":[]}`, http.StatusNoContent)
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "revoke", tc.ref, "--id", accountInviteULID)
			require.NoError(t, err)
			require.Equal(t, []string{"DELETE " + tc.base + "/" + accountInviteULID + "?"}, core.recorded())
			require.Equal(t, "✓ Revoked the invitation "+accountInviteULID+" to "+tc.name+" "+tc.ref+"\n", out)
		})
	}
}

// TestInviteRevoke_ByIDNotFound: an invitation that is already gone is the end
// state the user asked for, as every revoke verb treats a 404.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteRevoke_ByIDNotFound(t *testing.T) {
	for _, tc := range inviteScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newInviteCore(t, `{"invitations":[]}`, http.StatusNotFound)
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "revoke", tc.ref, "--id", accountInviteULID)
			require.NoError(t, err)
			require.Equal(t, "the invitation "+accountInviteULID+" to "+tc.name+" "+tc.ref+": no such invitation; nothing to revoke\n", out)
		})
	}
}

// TestInviteRevoke_ByEmailMatchesOnlyEmailInvitations: --email finds the open
// invitation for that address through the listing; an account invitation has
// no address and is never matched.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteRevoke_ByEmailMatchesOnlyEmailInvitations(t *testing.T) {
	for _, tc := range inviteScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			core, srv := newInviteCore(t, invitationsFixture(tc.resource), http.StatusNoContent)
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "revoke", tc.ref, "--email", "Dev@Example.com")
			require.NoError(t, err)
			require.Equal(t, []string{"GET " + tc.base + "?status=open", "DELETE " + tc.base + "/" + emailInviteULID + "?"}, core.recorded())
			require.Contains(t, out, "✓ Revoked the invitation for Dev@Example.com to "+tc.name+" "+tc.ref)
		})
	}
}

// TestInviteRevoke_RefusesBadFlagsLocally: one of --id and --email, never both,
// and an id that is not a ULID, are refused before any request.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestInviteRevoke_RefusesBadFlagsLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	for _, tc := range inviteScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "invite", "revoke", tc.ref)
			require.ErrorContains(t, err, "at least one of the flags in the group [id email] is required")
			_, _, err = runCoreCmd(t, tc.newCmd, srv.URL, "invite", "revoke", tc.ref, "--id", accountInviteULID, "--email", "dev@example.com")
			require.ErrorContains(t, err, "none of the others can be")
			_, _, err = runCoreCmd(t, tc.newCmd, srv.URL, "invite", "revoke", tc.ref, "--id", "github:alice")
			require.ErrorContains(t, err, `invalid --id "github:alice": an invitation ID is a ULID`)
		})
	}
}

// TestGrantAdd_RoleMismatchNamesTheInviteCommands: the refusal that says to
// revoke an open invitation first is followed by the commands that do it, for
// the scope the user named, as a typed grantee's error and in a picker run.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_RoleMismatchNamesTheInviteCommands(t *testing.T) {
	for _, tc := range addTargets() {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newAddCore(t, func(string) addReply {
				return replyProblem(http.StatusConflict, roleMismatchType, roleMismatchDetail)
			})
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args...)
			ref := tc.args[1]
			require.EqualError(t, err, roleMismatchDetail+"\n"+
				"Find the open invitation with `entire "+tc.name+" invite list "+ref+"`, then revoke it with `entire "+tc.name+" invite revoke "+ref+" --id <id>`.")
		})
	}

	t.Run("picker", func(t *testing.T) {
		srv, _ := pickerAddServer(t, pickerMembers("github:carol"), func(string) addReply {
			return replyProblem(http.StatusConflict, roleMismatchType, roleMismatchDetail)
		})
		pickAll(t, "github:carol")
		_, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
		require.Error(t, err)
		require.True(t, strings.HasPrefix(errOut,
			"✗ github:carol was not added to project "+pickerProjULID+": "+roleMismatchDetail+"\n"+
				"  Find the open invitation with `entire project invite list "+pickerProjULID+"`, then revoke it with `entire project invite revoke "+pickerProjULID+" --id <id>`.\n"), errOut)
	})

	t.Run("no hint for another refusal", func(t *testing.T) {
		_, srv := newAddCore(t, func(string) addReply { return replyProblem(http.StatusConflict, "", "already a member") })
		_, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "add", wiringProjULID, "github:alice", "--role", "reader")
		require.EqualError(t, err, "already a member")
	})
}

// TestInvitationViewRow pins the row a listing prints and how the invitee is
// chosen.
func TestInvitationViewRow(t *testing.T) {
	t.Parallel()
	v := invitationView{id: accountInviteULID, role: "writer", status: "open", accountID: inviteeAccount, expiresAt: time.Date(2026, 11, 20, 21, 42, 56, 0, time.UTC)}
	require.Equal(t, []string{accountInviteULID, inviteeAccount, "writer", "open", "2026-11-20"}, v.row())
	v.identity = coreapi.NewOptAccountIdentity(coreapi.AccountIdentity{Handles: []coreapi.AccountProfileHandle{{Provider: "github", Handle: "bob"}}})
	require.Equal(t, "github:bob", v.invitee(), "the first handle when none is primary")
	v.email = "dev@example.com"
	require.Equal(t, "dev@example.com", v.invitee())
}
