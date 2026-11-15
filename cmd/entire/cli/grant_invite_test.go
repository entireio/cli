package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// The adds invite an account outside the parent scope instead of granting it
// (ENT-2652). These drive `<noun> grant add` against an httptest core that
// answers the add with 201 (granted), 202 (invited) or a refusal.

const (
	inviteID       = "01M7BDT64K8BTHRT2996R6XNG2"
	invitedBody    = `{"$schema":"https://us.console.entire.io/api/v1/schemas/AccountInvited.json","status":"invited","invitation_id":"` + inviteID + `","expires_at":"2026-11-20T21:42:56.403044Z"}`
	membershipBody = `{"$schema":"https://us.console.entire.io/api/v1/schemas/Membership.json","id":"01HZX7QABCDEFGHJKMNPQRSTM1","accountId":"` + wiringGranteeULID + `","orgId":"` + wiringOrgULID + `","role":"member","status":"active","provider":"github","handle":"alice","createdAt":"2026-11-01T10:00:00Z"}`
	grantedBody    = `{"$schema":"https://us.console.entire.io/api/v1/schemas/GrantProjectAccessOutputBody.json","status":"granted"}`
)

// addReply is one canned answer to an add: status, content type and body.
type addReply struct {
	status      int
	contentType string
	body        string
	header      map[string]string
}

func replyJSON(status int, body string) addReply {
	return addReply{status: status, contentType: "application/json", body: body}
}

func replyProblem(status int, problemType, detail string) addReply {
	problem := map[string]any{"status": status, "title": http.StatusText(status), "detail": detail}
	if problemType != "" {
		problem["type"] = problemType
	}
	b, err := json.Marshal(problem)
	if err != nil {
		panic(err)
	}
	return addReply{status: status, contentType: "application/problem+json", body: string(b)}
}

// addTarget is one of the three add commands, addressed so it resolves without
// a lookup (org, project) or through the native path lookup (repo).
type addTarget struct {
	name     string
	newCmd   func() *cobra.Command
	args     []string // add <target> github:alice [--role]
	path     string   // the add's route
	granted  string   // a 201 body
	describe string   // how the target reads in output
	role     string   // the role the line names
}

func addTargets() []addTarget {
	return []addTarget{
		{"org", newOrgGrantCmd, []string{"add", wiringOrgULID, "github:alice"}, "/api/v1/orgs/" + wiringOrgULID + "/members", membershipBody, "org " + wiringOrgULID, "member"},
		{"project", newProjectGrantCmd, []string{"add", wiringProjULID, "github:alice", "--role", "writer"}, "/api/v1/projects/" + wiringProjULID + "/grants", grantedBody, "project " + wiringProjULID, "writer"},
		{"repo", newRepoGrantCmd, []string{"add", wiringRepoPath, "github:alice", "--role", "reader"}, "/api/v1/repos/" + wiringRepoULID + "/grants", `{"status":"granted"}`, "repo " + wiringRepoPath, "reader"},
	}
}

// addCore serves the handle lookup and repo path resolution an add makes, and
// answers the add itself from reply, keyed by the grantee's provider user id
// ("uid-<handle>"). Every add's path and body is recorded.
type addCore struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
}

func (a *addCore) recorded() ([]string, []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...), append([]map[string]any(nil), a.bodies...)
}

func newAddCore(t *testing.T, reply func(providerUserID string) addReply) (*addCore, *httptest.Server) {
	t.Helper()
	a := &addCore{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/identity/handles/"):
			handle := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			writeJSONResponse(t, w, http.StatusOK, &coreapi.ResolvedIdentity{
				AccountId: wiringGranteeULID, Provider: providerGitHub, Handle: handle, ProviderUserId: "uid-" + handle,
			})
		case strings.HasSuffix(r.URL.Path, "/repos/resolve"):
			writeJSONResponse(t, w, http.StatusOK, nativeResolution("acme/web", wiringRepoULID))
		case r.Method == http.MethodPost:
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read add body: %v", err)
				return
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode add body: %v", err)
				return
			}
			a.mu.Lock()
			a.paths = append(a.paths, r.URL.Path)
			a.bodies = append(a.bodies, body)
			a.mu.Unlock()
			uid, _ := body["providerUserId"].(string) //nolint:errcheck // absent reads as ""
			rep := reply(uid)
			for k, v := range rep.header {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", rep.contentType)
			w.WriteHeader(rep.status)
			if _, err := w.Write([]byte(rep.body)); err != nil {
				t.Errorf("write add reply: %v", err)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return a, srv
}

// TestGrantAdd_GrantedSendsTheOptInAndPrintsTheGrantedLine: every add allows
// an invitation, and a 201 prints exactly what the command printed before
// invitations existed. The org's 201 is today's Membership, so a core that
// has not been upgraded still reads as a grant.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_GrantedSendsTheOptInAndPrintsTheGrantedLine(t *testing.T) {
	for _, tc := range addTargets() {
		t.Run(tc.name, func(t *testing.T) {
			core, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusCreated, tc.granted) })
			out, errOut, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args...)
			require.NoError(t, err)
			require.Empty(t, errOut)
			require.Equal(t, "✓ Granted github:alice "+tc.role+" access to "+tc.describe+"\n", out)
			paths, bodies := core.recorded()
			require.Equal(t, []string{tc.path}, paths)
			require.Equal(t, true, bodies[0]["allow_invitation"])
		})
	}
}

// TestGrantAdd_InvitedPrintsTheInvitationAndItsExpiry: a 202 says the account
// was invited, not granted, names the invitation, and says when it expires.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_InvitedPrintsTheInvitationAndItsExpiry(t *testing.T) {
	for _, tc := range addTargets() {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusAccepted, invitedBody) })
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args...)
			require.NoError(t, err, "an invitation is a success")
			require.Equal(t, "✓ Invited github:alice to "+tc.describe+" as "+tc.role+" ("+inviteID+"). No access until they accept; the invitation expires 2026-11-20.\n", out)
			require.NotContains(t, out, "Granted")
		})
	}
}

// TestGrantAdd_JSONSaysWhichOutcome: --json emits the add's own body, so a
// grant and an invitation are told apart by status, and the invitation
// carries its id and expiry.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_JSONSaysWhichOutcome(t *testing.T) {
	for _, tc := range addTargets() {
		t.Run(tc.name+"/invited", func(t *testing.T) {
			_, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusAccepted, invitedBody) })
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, append(tc.args, "--json")...)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			require.Equal(t, "invited", got["status"])
			require.Equal(t, inviteID, got["invitation_id"])
			require.Equal(t, "2026-11-20T21:42:56Z", got["expires_at"], "ogen re-encodes date-time to the second")
		})
		t.Run(tc.name+"/granted", func(t *testing.T) {
			_, srv := newAddCore(t, func(string) addReply { return replyJSON(http.StatusCreated, tc.granted) })
			out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, append(tc.args, "--json")...)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			require.NotContains(t, got, "invitation_id")
			require.NotEqual(t, "invited", got["status"])
		})
	}
}

// TestGrantAdd_RefusalsAreCoresReasonOnOneLine: each refusal the invitation
// contract adds reaches the user as Core's detail, alone, on one line, and
// nothing reads as a grant or an invitation.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_RefusalsAreCoresReasonOnOneLine(t *testing.T) {
	refusals := map[string]addReply{
		"role mismatch":     replyProblem(http.StatusConflict, "https://entire.com/errors/invitation/role-mismatch", "an invitation with another role is open; revoke it first"),
		"already a member":  replyProblem(http.StatusConflict, "", "already a member"),
		"single sign-on":    replyProblem(http.StatusForbidden, "https://entire.com/errors/invitation/sso-required", "the organization requires single sign-on; this account cannot join by invitation and must sign in through the organization's single sign-on"),
		"invitation resent": withHeader(replyProblem(http.StatusTooManyRequests, "", "invitation was sent recently; wait before resending"), "Retry-After", "300"),
	}
	for _, tc := range addTargets() {
		for name, rep := range refusals {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				_, srv := newAddCore(t, func(string) addReply { return rep })
				out, _, err := runCoreCmd(t, tc.newCmd, srv.URL, tc.args...)
				require.Error(t, err)
				var problem struct {
					Detail string `json:"detail"`
				}
				require.NoError(t, json.Unmarshal([]byte(rep.body), &problem))
				require.Equal(t, problem.Detail, err.Error())
				require.Empty(t, out)
			})
		}
	}
}

func withHeader(r addReply, key, value string) addReply {
	r.header = map[string]string{key: value}
	return r
}

// TestGrantPicker_MixedOutcomes: a picker run whose grantees are granted,
// invited and refused reports each, names the refused grantee, tallies the
// set, and fails because of the refusal alone. A run that only grants and
// invites succeeds.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_MixedOutcomes(t *testing.T) {
	reply := func(uid string) addReply {
		switch uid {
		case "uid-alice":
			return replyJSON(http.StatusCreated, grantedBody)
		case "uid-bob":
			return replyJSON(http.StatusAccepted, invitedBody)
		default:
			return replyProblem(http.StatusConflict, "https://entire.com/errors/invitation/role-mismatch", "an invitation with another role is open; revoke it first")
		}
	}
	members := []coreapi.OrgMemberListItem{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
		member("github:carol", "acct-c"), member("github:dave", "acct-d"),
	}
	run := func(t *testing.T, picked []string, extra ...string) (string, string, []string, error) {
		t.Helper()
		var unused []string
		pool := pickerServer(t, pickerFixture{members: members}, &unused, nil)
		t.Cleanup(pool.Close)
		core, adds := newAddCore(t, reply)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost || strings.Contains(r.URL.Path, "/identity/handles/") {
				adds.Config.Handler.ServeHTTP(w, r)
				return
			}
			pool.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
			sel := make([]grantSelection, 0, len(picked))
			for _, h := range picked {
				sel = append(sel, grantSelection{handle: h, role: "reader"})
			}
			return sel, nil
		})
		out, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, extra...)
		paths, _ := core.recorded()
		return out, errOut, paths, err
	}
	project := "project " + pickerProjULID

	t.Run("granted, invited, refused, not attempted", func(t *testing.T) {
		out, _, paths, err := run(t, []string{"github:alice", "github:bob", "github:carol", "github:dave"})
		require.EqualError(t, err, "github:carol was not added to "+project+": an invitation with another role is open; revoke it first")
		require.Len(t, paths, 3, "the walk stops at the refusal")
		require.Equal(t, strings.Join([]string{
			"✓ Granted github:alice reader access to " + project,
			"✓ Invited github:bob to " + project + " as reader (" + inviteID + "). No access until they accept; the invitation expires 2026-11-20.",
			"1 granted, 1 invited, 1 failed, 1 not attempted (an invitation counts as done: access starts when it is accepted)",
		}, "\n")+"\n", out)
	})

	t.Run("granted and invited succeed", func(t *testing.T) {
		out, _, _, err := run(t, []string{"github:alice", "github:bob"})
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(out, "1 granted, 1 invited (an invitation counts as done: access starts when it is accepted)\n"), out)
	})

	t.Run("--json keeps the tally off stdout", func(t *testing.T) {
		out, errOut, _, err := run(t, []string{"github:alice", "github:bob", "github:carol"}, "--json")
		require.Error(t, err)
		var got []map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got), out)
		require.Len(t, got, 2)
		require.Equal(t, "granted", got[0]["status"])
		require.Equal(t, "invited", got[1]["status"])
		require.Equal(t, inviteID, got[1]["invitation_id"])
		require.Contains(t, errOut, fmt.Sprintf("%d granted, %d invited, %d failed", 1, 1, 1))
	})
}
