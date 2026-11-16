package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// pickerAddServer serves a project picker run whose pool is members, and
// answers each grantee's add with reply, keyed by provider user id
// ("uid-<handle>"). It returns the server and the adds' recorder.
func pickerAddServer(t *testing.T, members []coreapi.OrgMemberListItem, reply func(providerUserID string) addReply) (*httptest.Server, *addCore) {
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
	return srv, core
}

// pickAll answers the picker with every handle, each as reader.
func pickAll(t *testing.T, handles ...string) {
	t.Helper()
	capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
		sel := make([]grantSelection, 0, len(handles))
		for _, h := range handles {
			sel = append(sel, grantSelection{handle: h, role: "reader"})
		}
		return sel, nil
	})
}

func pickerMembers(handles ...string) []coreapi.OrgMemberListItem {
	out := make([]coreapi.OrgMemberListItem, 0, len(handles))
	for _, h := range handles {
		out = append(out, member(h, "acct-"+h))
	}
	return out
}

const (
	roleMismatchType   = "https://entire.com/errors/invitation/role-mismatch"
	roleMismatchDetail = "an invitation with another role is open; revoke it first"
)

// TestGrantPicker_RefusalsDoNotStopTheSet: a picker run attempts every
// grantee. Each one Core refuses is named on stderr with its reason, the
// others are granted or invited, the tally counts all four outcomes, and the
// command fails because someone was refused.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_RefusalsDoNotStopTheSet(t *testing.T) {
	reply := func(uid string) addReply {
		switch uid {
		case "uid-bob":
			return replyJSON(http.StatusAccepted, invitedBody)
		case "uid-carol":
			return replyProblem(http.StatusConflict, roleMismatchType, roleMismatchDetail)
		case "uid-erin":
			return replyProblem(http.StatusConflict, "", "already a member")
		default:
			return replyJSON(http.StatusCreated, grantedBody)
		}
	}
	handles := []string{"github:alice", "github:bob", "github:carol", "github:dave", "github:erin"}
	srv, core := pickerAddServer(t, pickerMembers(handles...), reply)
	pickAll(t, handles...)
	project := "project " + pickerProjULID

	out, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)

	var silent *SilentError
	require.ErrorAs(t, err, &silent, "the lines already printed are the report")
	require.EqualError(t, err, "2 of 5 grantees were not added to "+project)
	paths, _ := core.recorded()
	require.Len(t, paths, 5, "every grantee is attempted")
	require.Equal(t, strings.Join([]string{
		"✓ Granted github:alice reader access to " + project,
		"✓ Invited github:bob to " + project + " as reader (" + inviteID + "). No access until they accept; the invitation expires 2026-11-20.",
		"✓ Granted github:dave reader access to " + project,
		"2 granted, 1 invited, 2 refused (an invitation counts as done: access starts when it is accepted)",
	}, "\n")+"\n", out)
	require.True(t, strings.HasPrefix(errOut, strings.Join([]string{
		"✗ github:carol was not added to " + project + ": " + roleMismatchDetail,
		"✗ github:erin was not added to " + project + ": already a member",
	}, "\n")+"\n"), errOut)
}

// TestGrantPicker_FatalFailureStopsTheSet: a failure that would repeat for
// every grantee (here the caller may not manage the project) stops the set,
// and the tally counts who was not attempted.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_FatalFailureStopsTheSet(t *testing.T) {
	reply := func(uid string) addReply {
		switch uid {
		case "uid-alice":
			return replyJSON(http.StatusCreated, grantedBody)
		case "uid-bob":
			return replyProblem(http.StatusConflict, "", "already a member")
		default:
			return replyProblem(http.StatusForbidden, "", "permission denied")
		}
	}
	handles := []string{"github:alice", "github:bob", "github:carol", "github:dave", "github:erin"}
	srv, core := pickerAddServer(t, pickerMembers(handles...), reply)
	pickAll(t, handles...)
	project := "project " + pickerProjULID

	out, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)

	require.EqualError(t, err, "4 of 5 grantees were not added to "+project)
	paths, _ := core.recorded()
	require.Len(t, paths, 3, "the set stops at the 403")
	require.Equal(t, "✓ Granted github:alice reader access to "+project+"\n1 granted, 0 invited, 1 refused, 1 failed, 2 not attempted\n", out)
	require.Contains(t, errOut, "✗ github:bob was not added to "+project+": already a member\n✗ github:carol was not added to "+project+": permission denied\n")
}

// TestGrantPicker_GrantedAndInvitedSucceed: an invitation is a success, so a
// set that only grants and invites exits 0.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_GrantedAndInvitedSucceed(t *testing.T) {
	reply := func(uid string) addReply {
		if uid == "uid-bob" {
			return replyJSON(http.StatusAccepted, invitedBody)
		}
		return replyJSON(http.StatusCreated, grantedBody)
	}
	srv, _ := pickerAddServer(t, pickerMembers("github:alice", "github:bob"), reply)
	pickAll(t, "github:alice", "github:bob")

	out, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.Empty(t, errOut)
	require.True(t, strings.HasSuffix(out, "1 granted, 1 invited (an invitation counts as done: access starts when it is accepted)\n"), out)
}

// TestGrantPicker_MixedOutcomesInJSON: --json prints the array of what landed
// (grants and invitations, told apart by status) on stdout; refusals and the
// tally go to stderr; the exit still reports the refusal.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_MixedOutcomesInJSON(t *testing.T) {
	reply := func(uid string) addReply {
		switch uid {
		case "uid-alice":
			return replyJSON(http.StatusCreated, grantedBody)
		case "uid-carol":
			return replyJSON(http.StatusAccepted, invitedBody)
		default:
			return replyProblem(http.StatusConflict, "", "already a member")
		}
	}
	srv, _ := pickerAddServer(t, pickerMembers("github:alice", "github:bob", "github:carol"), reply)
	pickAll(t, "github:alice", "github:bob", "github:carol")

	out, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
	require.Error(t, err)
	var got []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got, 2)
	require.Equal(t, "granted", got[0]["status"])
	require.Equal(t, "invited", got[1]["status"])
	require.Equal(t, inviteID, got[1]["invitation_id"])
	require.Contains(t, errOut, "✗ github:bob was not added to project "+pickerProjULID+": already a member\n")
	require.Contains(t, errOut, "1 granted, 1 invited, 1 refused")
}

func TestGrantFailureIsFatal(t *testing.T) {
	t.Parallel()
	problem := func(status int, problemType string) error {
		e := &coreapi.ErrorModelStatusCode{StatusCode: status}
		if problemType != "" {
			u, err := url.Parse(problemType)
			if err != nil {
				t.Fatal(err)
			}
			e.Response.Type = coreapi.NewOptURI(*u)
		}
		return e
	}
	for name, tc := range map[string]struct {
		err   error
		fatal bool
	}{
		"401 not authenticated":        {problem(http.StatusUnauthorized, ""), true},
		"403 no permission":            {problem(http.StatusForbidden, ""), true},
		"403 sso-required":             {problem(http.StatusForbidden, problemTypeSSORequired), false},
		"404 grantee account":          {problem(http.StatusNotFound, ""), false},
		"409 already a member":         {problem(http.StatusConflict, ""), false},
		"409 role mismatch":            {problem(http.StatusConflict, roleMismatchType), false},
		"421 not followed":             {problem(http.StatusMisdirectedRequest, ""), true},
		"422 invalid":                  {problem(http.StatusUnprocessableEntity, ""), false},
		"429 cooldown":                 {problem(http.StatusTooManyRequests, ""), false},
		"500":                          {problem(http.StatusInternalServerError, ""), true},
		"503":                          {problem(http.StatusServiceUnavailable, ""), true},
		"transport":                    {&url.Error{Op: "Post", URL: "https://core", Err: errors.New("connection refused")}, true},
		"undecodable error body":       {errors.New("decode response: default (code 502): invalid content type"), true},
		"cancelled":                    {context.Canceled, true},
		"handle names no identity":     {&granteeUnresolvedError{errors.New(`no github identity for handle "ghost"`)}, false},
		"wrapped grantee API conflict": {fmt.Errorf("add: %w", problem(http.StatusConflict, "")), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.fatal, grantFailureIsFatal(tc.err))
		})
	}
}
