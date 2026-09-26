package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// The picker's pool is the owning org's membership minus whoever already holds
// the target. These tests drive it through cobra against an httptest control
// plane, with the form seam swapped: `go test` is non-interactive, so the real
// forms are unreachable and the refusing paths are what run by default.

const (
	pickerOrgULID  = "01HZX7QABCDEFGHJKMNPQRSTW0"
	pickerProjULID = "01HZX7QABCDEFGHJKMNPQRSTW1"
	pickerRepoULID = "01HZX7QABCDEFGHJKMNPQRSTW2"
)

// Grantee ids are ULID-shaped because the real ones are, and the remove flow
// routes a ULID ref to the typed-id revoke route. A placeholder like "acct-a"
// would quietly take the by-handle path instead and test the wrong thing.
var (
	acctAlice = holder{"01HZX7QABCDEFGHJKMNPQRSTA1", "github:alice"}
	acctBob   = holder{"01HZX7QABCDEFGHJKMNPQRSTB2", "github:bob"}
)

// holder is one account that already holds a target, in the fixture.
type holder struct {
	id     string
	handle string
}

// pickerFixture is one control plane's answers: the org behind the target, its
// members, and who already holds the target.
type pickerFixture struct {
	ownerType coreapi.ProjectOwnerType
	members   []coreapi.Membership
	held      []holder // accounts holding the target directly
	// viaProject holds the grantees a repo carries through its project. Listing
	// returns them alongside the direct rows, and the add pool must NOT subtract
	// them: they hold no grant on the repo itself, so granting one here is a
	// real action. Only the direct rows are subtracted.
	viaProject []holder
	// withOwnerRow adds the synthetic row for the owning org, which every real
	// listing carries and neither picker may offer.
	withOwnerRow bool
}

// inactive is a member who has not joined, so no provider identity resolves for
// them and they cannot be granted anything.
func inactive(handle, accountID string) coreapi.Membership {
	m := member(handle, accountID)
	m.Status = "invited"
	return m
}

func member(handle, accountID string) coreapi.Membership {
	return coreapi.Membership{
		AccountId: accountID,
		Handle:    coreapi.NewOptString(handle),
		Provider:  coreapi.NewOptString(providerGitHub),
		Role:      "member",
		Status:    "active",
	}
}

func (f pickerFixture) projectGrants() []coreapi.ProjectGrant {
	rows := make([]coreapi.ProjectGrant, 0, len(f.held))
	for _, h := range f.held {
		rows = append(rows, coreapi.ProjectGrant{GranteeId: h.id, GranteeType: granteeTypeAccount, GranteeName: coreapi.NewOptString(h.handle), Role: "writer", Source: "direct"})
	}
	if f.withOwnerRow {
		rows = append(rows, coreapi.ProjectGrant{GranteeId: pickerOrgULID, GranteeType: "org", GranteeName: coreapi.NewOptString("acme"), Role: "owner", Source: "owner"})
	}
	return rows
}

func (f pickerFixture) repoGrants() []coreapi.RepoGrant {
	rows := make([]coreapi.RepoGrant, 0, len(f.held)+len(f.viaProject))
	for _, h := range f.held {
		rows = append(rows, coreapi.RepoGrant{GranteeId: h.id, GranteeType: granteeTypeAccount, GranteeName: coreapi.NewOptString(h.handle), Role: "writer", Source: "direct"})
	}
	for _, h := range f.viaProject {
		rows = append(rows, coreapi.RepoGrant{GranteeId: h.id, GranteeType: granteeTypeAccount, GranteeName: coreapi.NewOptString(h.handle), Role: "writer", Source: "project:widgets"})
	}
	if f.withOwnerRow {
		rows = append(rows, coreapi.RepoGrant{GranteeId: pickerOrgULID, GranteeType: "org", GranteeName: coreapi.NewOptString("acme"), Role: "owner", Source: "owner"})
	}
	return rows
}

// pickerServer serves every lookup the picker makes, and records grant POSTs as
// "<path> <providerUserId> <role>". grantStatus, when set, chooses the status
// code for the nth grant so a mid-walk failure can be staged.
//
// Failures are reported with t.Errorf rather than require, which must not be
// called from a handler goroutine.
func pickerServer(t *testing.T, f pickerFixture, grants *[]string, grantStatus func(i int) int) *httptest.Server {
	t.Helper()
	ownerType := f.ownerType
	if ownerType == "" {
		ownerType = coreapi.ProjectOwnerTypeOrg
	}
	write := func(w http.ResponseWriter, code int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := printJSON(w, payload); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A /et/<project>/<repo> ref resolves through repos/resolve, which is a
		// POST like the grant routes are — so it is answered before them rather
		// than recorded as a grant. The requested name is echoed back, because
		// the resolver matches on it and this fixture answers for whatever repo
		// path a test addresses.
		if strings.HasSuffix(r.URL.Path, "/repos/resolve") {
			var body coreapi.ResolveReposInputBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Repositories) == 0 {
				t.Errorf("decode resolve body: %v", err)
				return
			}
			write(w, http.StatusOK, nativeResolution(body.Repositories[0].FullName, pickerRepoULID))
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				ProviderUserID string `json:"providerUserId"`
				Role           string `json:"role"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode grant body: %v", err)
				return
			}
			i := len(*grants)
			*grants = append(*grants, r.URL.Path+" "+body.ProviderUserID+" "+body.Role)
			// Anything from 400 up is the staged failure; every other answer
			// is the ordinary 201 the grant routes return.
			if grantStatus != nil {
				if code := grantStatus(i); code >= http.StatusBadRequest {
					w.WriteHeader(code)
					return
				}
			}
			write(w, http.StatusCreated, map[string]string{"status": "ok"})
			return
		}
		if r.Method == http.MethodDelete {
			*grants = append(*grants, "DELETE "+r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/identity/handles/"):
			// The provider user id is derived from the handle so a test can
			// tell the grants apart by who they were for.
			seg := strings.Split(strings.Trim(path, "/"), "/")
			handle := seg[len(seg)-1]
			write(w, http.StatusOK, &coreapi.ResolvedIdentity{
				AccountId: "acct-" + handle, Provider: providerGitHub,
				Handle: handle, ProviderUserId: "uid-" + handle,
			})
		case strings.HasSuffix(path, "/projects"):
			// The by-name project lookup behind a /et/<project>/<repo> ref.
			write(w, http.StatusOK, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
				ID: pickerProjULID, Name: "widgets", OwnerId: pickerOrgULID, OwnerType: ownerType,
			})})
		case strings.HasSuffix(path, "/repos"):
			write(w, http.StatusOK, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
				ID: pickerRepoULID, Name: "web", OwningProjectId: pickerProjULID,
			})})
		case strings.HasSuffix(path, "/repos/"+pickerRepoULID):
			write(w, http.StatusOK, &coreapi.Repo{ID: pickerRepoULID, Name: "web", OwningProjectId: pickerProjULID})
		case strings.HasSuffix(path, "/projects/"+pickerProjULID):
			write(w, http.StatusOK, &coreapi.Project{ID: pickerProjULID, Name: "widgets", OwnerId: pickerOrgULID, OwnerType: ownerType})
		case strings.HasSuffix(path, "/members") && strings.Contains(path, "/orgs/"):
			write(w, http.StatusOK, &coreapi.ListOrgMembersOutputBody{Members: f.members})
		case strings.HasSuffix(path, "/members"):
			write(w, http.StatusOK, &coreapi.ListProjectMembersOutputBody{Members: f.projectGrants()})
		case strings.HasSuffix(path, "/grants"):
			write(w, http.StatusOK, &coreapi.ListRepoGrantsOutputBody{Grants: f.repoGrants()})
		default:
			t.Errorf("unexpected GET %s", path)
		}
	}))
}

// capturePicker puts the command on the interactive path and swaps the form
// seam, recording what was offered and answering with the given selections.
//
// ENTIRE_TEST_TTY=1 is what gets past the gate: `go test` is non-interactive, so
// without it every one of these commands refuses before reaching a picker. The
// forms themselves never run, because the seam replaces them.
func capturePicker(t *testing.T, answer func(offered []grantCandidate, known []string, fixedRole string) ([]grantSelection, error)) *[]grantCandidate {
	t.Helper()
	t.Setenv("ENTIRE_TEST_TTY", "1")
	var offered []grantCandidate
	prev := grantPicker
	grantPicker = func(_ *cobra.Command, _ grantPickerTarget, candidates []grantCandidate, known []string, fixedRole string) ([]grantSelection, error) {
		offered = candidates
		return answer(candidates, known, fixedRole)
	}
	t.Cleanup(func() { grantPicker = prev })
	return &offered
}

// captureRemovePicker puts the command on the interactive path and swaps the
// remove form seam, recording what was offered and answering with the rows to
// revoke.
//
// ENTIRE_TEST_TTY=1 puts the confirmation in play as well, so it stubs that too
// and answers yes; a test that cares about declining says so itself.
func captureRemovePicker(t *testing.T, answer func(offered []grantCandidate) []grantCandidate) *[]grantCandidate {
	t.Helper()
	t.Setenv("ENTIRE_TEST_TTY", "1")
	var offered []grantCandidate
	prev := removePicker
	removePicker = func(_ *cobra.Command, _ grantPickerTarget, candidates []grantCandidate) ([]grantCandidate, error) {
		offered = candidates
		return answer(candidates), nil
	}
	prevConfirm := revokeConfirmed
	revokeConfirmed = func(*cobra.Command, grantPickerTarget, []grantCandidate) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { removePicker, revokeConfirmed = prev, prevConfirm })
	return &offered
}

// labels is what the picker shows, as opposed to what it acts on.
func labels(cs []grantCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.label
	}
	return out
}

func handles(cs []grantCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ref
	}
	return out
}

// TestGrantPicker_ExcludesDirectHoldersButOffersInheritedOnes is the pool's
// rule, and the two halves were each wrong on their own in an earlier version.
//
// A member with a DIRECT grant on the target has nothing to add here, so they
// are left out; changing their role is the typed form's job. A member who holds
// the target only through its PROJECT has no grant on this target at all, so
// granting one is a real action — it pins the role on this repo instead of
// following the project's — and leaving them out emptied the pool on every repo
// whose project already covered the org.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_ExcludesDirectHoldersButOffersInheritedOnes(t *testing.T) {
	members := []coreapi.Membership{
		member("github:alice", "acct-a"),
		member("github:bob", "acct-b"),
		member("github:carol", "acct-c"),
	}
	for name, tc := range map[string]struct {
		newCmd  func() *cobra.Command
		ref     string
		fixture pickerFixture
		want    []string
	}{
		"project/a direct holder is left out": {
			newProjectGrantCmd, pickerProjULID,
			pickerFixture{members: members, held: []holder{{"acct-b", "github:bob"}}},
			[]string{"github:alice", "github:carol"},
		},
		"repo/a direct holder is left out": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{members: members, held: []holder{{"acct-a", "github:alice"}}},
			[]string{"github:bob", "github:carol"},
		},
		"repo/an inherited holder is offered": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{members: members, viaProject: []holder{{"acct-c", "github:carol"}}},
			[]string{"github:alice", "github:bob", "github:carol"},
		},
		"repo/a direct grant wins over the inherited row for the same account": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{
				members:    members,
				held:       []holder{{"acct-a", "github:alice"}},
				viaProject: []holder{{"acct-a", "github:alice"}},
			},
			[]string{"github:bob", "github:carol"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var grants []string
			srv := pickerServer(t, tc.fixture, &grants, nil)
			t.Cleanup(srv.Close)
			offered := capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
				return []grantSelection{{handle: cs[0].ref, role: "reader"}}, nil
			})

			_, _, err := runPickerCmd(t, tc.newCmd, srv.URL, tc.ref)
			require.NoError(t, err)
			// Members holding nothing come first: adding is the common case.
			require.Equal(t, tc.want, labels(*offered))
		})
	}
}

// TestGrantPicker_AnEmptyPoolSucceeds: having nobody to add is not a failure.
// Nothing went wrong, nothing is left for the user to fix, and in the common
// case the state they wanted already holds — the same reasoning that makes
// revoking an already-revoked grant a success rather than a 404. So each of
// these reports and exits 0.
//
// The message still distinguishes them, since "everyone already has a grant",
// "this org has no members" and "none of its members can be addressed" are
// different answers to "who can I add?".
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_AnEmptyPoolSucceeds(t *testing.T) {
	for name, tc := range map[string]struct {
		fixture pickerFixture
		want    string
	}{
		"everyone already holds a direct grant": {
			pickerFixture{
				members: []coreapi.Membership{member("github:alice", "acct-a")},
				held:    []holder{{"acct-a", "github:alice"}},
			},
			"every member of the org owning project " + pickerProjULID + " already has a grant on it",
		},
		"the org has no members": {
			pickerFixture{},
			"project " + pickerProjULID + " has no org members to choose from",
		},
		"no member can be addressed": {
			pickerFixture{members: []coreapi.Membership{inactive("github:alice", "acct-a")}},
			"no member of the org owning project " + pickerProjULID + " can be granted access here",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var grants []string
			srv := pickerServer(t, tc.fixture, &grants, nil)
			t.Cleanup(srv.Close)
			capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
				t.Error("the picker must not open with nobody to add")
				return nil, nil
			})

			out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
			require.NoError(t, err, "an empty pool is not an error")
			require.Contains(t, out, tc.want)
			require.Empty(t, grants)
		})
	}
}

// TestGrantPicker_EmptyPoolWithJSONStaysParseable: the reason is human output,
// so under --json it moves to stderr and stdout gets the empty array that "no
// grants were made" means there. Printing the sentence on stdout would hand a
// caller something it cannot parse.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_EmptyPoolWithJSONStaysParseable(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{
		members: []coreapi.Membership{member("github:alice", "acct-a")},
		held:    []holder{{"acct-a", "github:alice"}},
	}, &grants, nil)
	t.Cleanup(srv.Close)
	capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
		t.Error("the picker must not open with nobody to add")
		return nil, nil
	})

	out, errOut, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &arr))
	require.Empty(t, arr)
	require.Contains(t, errOut, "already has a grant on it")
}

// TestGrantPicker_SelectingNobodyIsACleanStop: confirming an empty selection is
// a decision not to grant anything. It used to exit 1 with no message at all,
// which is the worst of both.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_SelectingNobodyIsACleanStop(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{
		members: []coreapi.Membership{member("github:alice", "acct-a")},
	}, &grants, nil)
	t.Cleanup(srv.Close)
	capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
		return nil, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.Empty(t, grants)
	require.NotContains(t, out, "✓")
}

// TestGrantPicker_UngrantableMembersAreDropped: a member with no handle or one
// who has not joined cannot be resolved to the (provider, providerUserId) pair
// the grant routes need, so offering them would produce a selection that fails
// at the grant.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_UngrantableMembersAreDropped(t *testing.T) {
	noHandle := member("", "acct-x")
	noHandle.Handle = coreapi.OptString{}
	invited := member("github:pending", "acct-y")
	invited.Status = "invited"

	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), noHandle, invited,
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	offered := capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		return []grantSelection{{handle: cs[0].ref, role: "reader"}}, nil
	})

	_, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.Equal(t, []string{"github:alice"}, handles(*offered))
}

// TestGrantPicker_PerGranteeRoles pins that each selection carries its own role
// rather than one role being applied to the set, and that the grants are issued
// in the order chosen.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PerGranteeRoles(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	capturePicker(t, func(_ []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		return []grantSelection{
			{handle: "github:alice", role: "reader"},
			{handle: "github:bob", role: "admin"},
		}, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/api/v1/projects/" + pickerProjULID + "/grants uid-alice reader",
		"/api/v1/projects/" + pickerProjULID + "/grants uid-bob admin",
	}, grants)
	require.Contains(t, out, "✓ Granted github:alice reader access to project "+pickerProjULID)
	require.Contains(t, out, "✓ Granted github:bob admin access to project "+pickerProjULID)
}

// TestGrantPicker_FixedRoleIsNotPrompted: --role decides the roles, so the
// picker is handed it rather than asked for one, and every grant carries it.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_FixedRoleIsNotPrompted(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	var gotFixed string
	capturePicker(t, func(cs []grantCandidate, _ []string, fixedRole string) ([]grantSelection, error) {
		gotFixed = fixedRole
		out := make([]grantSelection, len(cs))
		for i, c := range cs {
			out[i] = grantSelection{handle: c.ref, role: fixedRole}
		}
		return out, nil
	})

	_, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--role", "admin")
	require.NoError(t, err)
	require.Equal(t, "admin", gotFixed)
	require.Equal(t, []string{
		"/api/v1/projects/" + pickerProjULID + "/grants uid-alice admin",
		"/api/v1/projects/" + pickerProjULID + "/grants uid-bob admin",
	}, grants)
}

// TestGrantPicker_PartialFailureStopsAndReports: a failure part-way through
// leaves the earlier grants in place and says so, rather than rolling back a
// server-side change the CLI cannot undo or swallowing the error.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PartialFailureStopsAndReports(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"), member("github:carol", "acct-c"),
	}}, &grants, func(i int) int {
		if i == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	t.Cleanup(srv.Close)
	capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		out := make([]grantSelection, len(cs))
		for i, c := range cs {
			out[i] = grantSelection{handle: c.ref, role: "reader"}
		}
		return out, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.Error(t, err)
	require.Contains(t, out, "github:alice")
	require.NotContains(t, out, "github:carol", "the walk must stop at the failure, not carry on")
	require.Len(t, grants, 2, "the third grant is never attempted")
}

// TestGrantPicker_PartialFailureIsReportedInJSONToo: --json replaces the
// confirmation lines, so without this the grants that landed before a failure
// would be invisible to exactly the callers parsing the output. The shape stays
// the array a picked set always produces, carrying only what succeeded.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PartialFailureIsReportedInJSONToo(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, func(i int) int {
		if i == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusCreated
	})
	t.Cleanup(srv.Close)
	capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		out := make([]grantSelection, len(cs))
		for i, c := range cs {
			out[i] = grantSelection{handle: c.ref, role: "reader"}
		}
		return out, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
	require.Error(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &arr))
	require.Len(t, arr, 1, "the one grant that landed is reported, the failed one is not")
}

// TestGrantPicker_NoPoolExistsIsAnError covers the case an empty pool is not:
// a target owned by an account has no membership list anywhere, so the picker
// cannot run at all and the user has to name a grantee. That is a different
// thing from an org whose members are all granted already, which succeeds — see
// TestGrantPicker_AnEmptyPoolSucceeds.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_NoPoolExistsIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		newCmd  func() *cobra.Command
		ref     string
		fixture pickerFixture
		want    string
	}{
		"project owned by an account": {
			newProjectGrantCmd, pickerProjULID,
			pickerFixture{ownerType: coreapi.ProjectOwnerTypeAccount},
			"project " + pickerProjULID + " is owned by an account, so it has no member list to choose from",
		},
		"repo whose project is owned by an account": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{ownerType: coreapi.ProjectOwnerTypeAccount},
			"repo " + wiringRepoPath + " is in project widgets, which is owned by an account",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var grants []string
			srv := pickerServer(t, tc.fixture, &grants, nil)
			t.Cleanup(srv.Close)
			capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
				t.Error("picker must not open when there is nobody to offer")
				return nil, nil
			})

			_, _, err := runPickerCmd(t, tc.newCmd, srv.URL, tc.ref)
			require.ErrorContains(t, err, tc.want)
			// Every refusal still names the form that always works, because a
			// grantee never has to be an org member.
			require.ErrorContains(t, err, "pass a grantee as provider:handle")
			require.Empty(t, grants)
		})
	}
}

// TestGrantAdd_NoGranteeIsRefusedBeforeAnyRequest: without a terminal the
// candidate list has no use, so the refusal is decided from the command line
// alone and costs no lookup.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_NoGranteeIsRefusedBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for name, tc := range map[string]struct {
		newCmd func() *cobra.Command
		ref    string
	}{
		"project": {newProjectGrantCmd, wiringProjULID},
		"repo":    {newRepoGrantCmd, wiringRepoPath},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "add", tc.ref)
			require.ErrorContains(t, err, "no grantee given; pass a grantee as provider:handle")
		})
	}
}

// TestOrgGrantAdd_HasNoPicker: org membership has no enumerable pool of
// candidates — everyone not already a member is, by definition, absent from the
// only list there is — so `org grant add` still requires both arguments.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgGrantAdd_HasNoPicker(t *testing.T) {
	require.Nil(t, orgGrantTarget.candidates)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgGrantCmd, srv.URL, "add", wiringOrgULID)
	require.ErrorContains(t, err, "accepts 2 arg(s), received 1")
}

// runPickerCmd runs `add <ref> [flags]` — the grantee-omitted form — against srv.
func runPickerCmd(t *testing.T, newCmd func() *cobra.Command, srvURL, ref string, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCoreCmd(t, newCmd, srvURL, append([]string{"add", ref}, extra...)...)
}

// TestGrantPicker_SoleCandidateIsStillOffered: a picker elsewhere in the CLI
// auto-picks when only one choice exists (selectPlacement returns the lone
// cluster without prompting), and that is right for choosing where to read
// from. This one writes access, so the single eligible person is still shown
// and still has to be chosen.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_SoleCandidateIsStillOffered(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{member("github:alice", "acct-a")}}, &grants, nil)
	t.Cleanup(srv.Close)
	opened := false
	capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		opened = true
		require.Len(t, cs, 1)
		return []grantSelection{{handle: cs[0].ref, role: "reader"}}, nil
	})

	_, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.True(t, opened, "the lone candidate must be chosen, not assumed")
}

// TestGrantAdd_JSONShapeFollowsTheInvocation: a grantee named on the command
// line is one mutation and emits the bare wire object, the shape every other
// mutation's --json emits and the one `grant add` emitted before the picker
// existed — so the scripted form neither breaks nor leaves this the only
// command in the CLI answering a mutation with an array. The picker grants a
// set and emits an array, including when it is empty, so a caller reading it
// never has to branch.
//
// Deciding on the outcome instead would make a picker run that granted one
// person indistinguishable from a typed one.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantAdd_JSONShapeFollowsTheInvocation(t *testing.T) {
	t.Run("a grantee named on the command line is an object", func(t *testing.T) {
		var grants []string
		srv := pickerServer(t, pickerFixture{}, &grants, nil)
		t.Cleanup(srv.Close)

		out, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL,
			"add", pickerProjULID, "github:alice", "--role", "reader", "--json")
		require.NoError(t, err)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &obj))
		require.NotEmpty(t, grants)
	})

	t.Run("a picked set is an array", func(t *testing.T) {
		var grants []string
		srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
			member("github:alice", "acct-a"), member("github:bob", "acct-b"),
		}}, &grants, nil)
		t.Cleanup(srv.Close)
		capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
			out := make([]grantSelection, len(cs))
			for i, c := range cs {
				out[i] = grantSelection{handle: c.ref, role: "reader"}
			}
			return out, nil
		})

		out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
		require.NoError(t, err)
		var arr []map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &arr))
		require.Len(t, arr, 2)
		require.NotContains(t, out, "✓", "--json replaces the confirmation lines")
	})

	t.Run("a picker that granted one is still an array", func(t *testing.T) {
		var grants []string
		srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
			member("github:alice", "acct-a"),
		}}, &grants, nil)
		t.Cleanup(srv.Close)
		capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
			return []grantSelection{{handle: cs[0].ref, role: "reader"}}, nil
		})

		out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
		require.NoError(t, err)
		var arr []map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &arr))
		require.Len(t, arr, 1, "the shape is the invocation's, not the outcome's")
	})
}

// TestRemovePicker_OrgHasAPoolToo: the add side cannot offer anything for an
// org, because everyone eligible is absent from the only list there is. Remove
// is the opposite — the members to remove ARE that list — so org gets a picker
// where add does not.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_OrgHasAPoolToo(t *testing.T) {
	require.NotNil(t, orgGrantTarget.holders, "remove has a pool where add has none")

	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	offered := captureRemovePicker(t, func(cs []grantCandidate) []grantCandidate { return []grantCandidate{cs[1]} })

	out, _, err := runCoreCmd(t, newOrgGrantCmd, srv.URL, "remove", pickerOrgULID)
	require.NoError(t, err)
	require.Equal(t, []string{"github:alice", "github:bob"}, labels(*offered))
	// Org members are addressed by handle: there is no typed-id revoke route.
	require.Equal(t, "github:bob", (*offered)[1].ref)
	require.Contains(t, out, "✓ Revoked github:bob from org "+pickerOrgULID)
}

// TestRemovePicker_ReportsTheNameItShowed: a project or repo row is revoked by
// account ULID, which needs no handle lookup and survives a rename, but the
// user chose a name off a list and the confirmation has to say that name back.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_ReportsTheNameItShowed(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{held: []holder{acctAlice}}, &grants, nil)
	t.Cleanup(srv.Close)
	captureRemovePicker(t, func(cs []grantCandidate) []grantCandidate { return []grantCandidate{cs[0]} })

	out, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", pickerProjULID)
	require.NoError(t, err)
	require.Contains(t, out, "✓ Revoked github:alice from project "+pickerProjULID)
	require.NotContains(t, out, "acct-a", "the id it acted on is not what the user picked")
}

// TestRemovePicker_EmptyPoolIsAnError: the user asked to revoke something and
// nothing was revoked, so this is not a quiet success.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_EmptyPoolIsAnError(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{withOwnerRow: true}, &grants, nil)
	t.Cleanup(srv.Close)
	captureRemovePicker(t, func([]grantCandidate) []grantCandidate {
		t.Error("the picker must not open with nothing to offer")
		return nil
	})

	_, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", pickerProjULID)
	require.ErrorContains(t, err, "project "+pickerProjULID+" has no grants that can be revoked here")
}

// TestGrantRemove_NoGranteeIsRefusedBeforeAnyRequest: without a terminal the
// list of holders has no use, so the refusal costs no lookup and names the one
// form a grantee takes.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_NoGranteeIsRefusedBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for name, tc := range map[string]struct {
		newCmd func() *cobra.Command
		ref    string
		want   string
	}{
		"org":     {newOrgGrantCmd, wiringOrgULID, "entire org grant remove " + wiringOrgULID + " github:alice"},
		"project": {newProjectGrantCmd, wiringProjULID, "entire project grant remove " + wiringProjULID + " github:alice"},
		"repo":    {newRepoGrantCmd, wiringRepoPath, "entire repo grant remove " + wiringRepoPath + " github:alice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "remove", tc.ref)
			require.ErrorContains(t, err, "no grantee given; ")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestGrantRemove_NeedsNoConfirmationBypass pins the shape of the confirmation:
// it belongs to the picker, so a typed grantee revokes unprompted and there is
// no flag to bypass anything. `delete` refuses instead, because a deleted
// resource is gone, while a revoked grant is one command from being restored.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_NeedsNoConfirmationBypass(t *testing.T) {
	var revoked string
	srv := httptest.NewServer(grantWiringHandler(t,
		func(_, path string) { revoked = path },
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
	))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", wiringProjULID, "github:alice")
	require.NoError(t, err)
	require.Contains(t, revoked, "/grants/account/github/12345")
	require.Contains(t, out, "✓ Revoked github:alice")

	// No bypass exists, on any of the three, because nothing needs bypassing.
	for name, newCmd := range map[string]func() *cobra.Command{
		"org": newOrgGrantCmd, "project": newProjectGrantCmd, "repo": newRepoGrantCmd,
	} {
		t.Run(name+" has no --force", func(t *testing.T) {
			require.Nil(t, newCmd().Commands()[2].Flags().Lookup("force"))
		})
	}
}

// TestRevokeConfirmation_NamesEveryGrantee: a prompt that summarised several
// revokes as a count alone would hide who is in the set, so the count is the
// title and the names are listed under it. One grantee needs no list and reads
// as a sentence.
func TestRevokeConfirmation_NamesEveryGrantee(t *testing.T) {
	t.Parallel()
	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}

	label, detail := revokeConfirmation(pt, []grantCandidate{{ref: "x", label: "github:alice"}})
	require.Equal(t, "github:alice from project widgets", label)
	require.Empty(t, detail, "a single grantee needs no list under it")

	label, detail = revokeConfirmation(pt, []grantCandidate{
		{ref: "x", label: "github:alice"},
		{ref: "y", label: "github:bob"},
	})
	require.Equal(t, "2 grants on project widgets", label)
	require.Contains(t, detail, "github:alice")
	require.Contains(t, detail, "github:bob")

	// The single-grantee title is read as a sentence — "Revoke <label>?" — so
	// the role it carries has to be punctuated into it rather than spaced after
	// it, which would run "admin" straight into "from".
	label, _ = revokeConfirmation(pt, []grantCandidate{{ref: "x", label: "github:alice", role: "admin"}})
	require.Equal(t, "github:alice (admin) from project widgets", label)

	_, detail = revokeConfirmation(pt, []grantCandidate{
		{ref: "x", label: "github:alice", role: "admin"},
		{ref: "y", label: "github:bob", role: "reader"},
	})
	require.Contains(t, detail, "github:alice (admin)")
	require.Contains(t, detail, "github:bob (reader)")
}

// TestGrantRemove_DecliningRevokesNothing is what the confirmation is for: a
// no at the prompt leaves every grant in place and exits cleanly, rather than
// revoking part of the set or reporting a failure.
//
// Not parallel: swaps the activeCoreClient, removePicker and revokeConfirmed seams.
func TestGrantRemove_DecliningRevokesNothing(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{held: []holder{acctAlice, acctBob}}, &grants, nil)
	t.Cleanup(srv.Close)
	captureRemovePicker(t, func(cs []grantCandidate) []grantCandidate {
		return []grantCandidate{cs[0], cs[1]}
	})

	var asked []grantCandidate
	prev := revokeConfirmed
	revokeConfirmed = func(_ *cobra.Command, _ grantPickerTarget, picked []grantCandidate) (bool, error) {
		asked = picked
		return false, nil
	}
	t.Cleanup(func() { revokeConfirmed = prev })

	out, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", pickerProjULID)
	require.NoError(t, err, "declining is a clean stop, not a failure")
	require.Empty(t, grants, "nothing may be revoked after a no")
	require.NotContains(t, out, "✓")
	// It is asked once for the whole set, naming both, rather than per grantee.
	require.Equal(t, []string{"github:alice", "github:bob"}, labels(asked))
}

// TestGrantRemove_ConfirmationIsSkippedWithoutATerminal is the non-interactive
// half of TestGrantRemove_ATypedGranteeIsNeverPrompted: the same typed revoke
// goes through untouched with no terminal at all. Together they pin that the
// outcome does not depend on terminal detection.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_ConfirmationIsSkippedWithoutATerminal(t *testing.T) {
	asked := false
	prev := revokeConfirmed
	revokeConfirmed = func(*cobra.Command, grantPickerTarget, []grantCandidate) (bool, error) {
		asked = true
		return true, nil
	}
	t.Cleanup(func() { revokeConfirmed = prev })

	var revoked string
	srv := httptest.NewServer(grantWiringHandler(t,
		func(_, path string) { revoked = path },
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
	))
	t.Cleanup(srv.Close)

	// No ENTIRE_TEST_TTY here, so CanPromptInteractively is false.
	_, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", wiringProjULID, "github:alice")
	require.NoError(t, err)
	require.False(t, asked, "a script is never asked a question it cannot answer")
	require.Contains(t, revoked, "/grants/account/github/12345")
}

// TestGrantAdd_TypedGranteeIsOnlyAskedForARole covers the one user-visible
// change on the typed path: `project grant add widgets github:alice` with no
// --role used to fail cobra's required-flag check and now prompts for one.
//
// What must hold is that the grantee is carried into the picker rather than
// re-chosen there — the pool is never fetched and the multi-select never opens.
// Passing nil instead of the typed grantee would discard it and open the full
// picker, which nothing else here would notice.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantAdd_TypedGranteeIsOnlyAskedForARole(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)

	var gotKnown []string
	var gotOffered []grantCandidate
	t.Setenv("ENTIRE_TEST_TTY", "1")
	prev := grantPicker
	grantPicker = func(_ *cobra.Command, _ grantPickerTarget, candidates []grantCandidate, known []string, _ string) ([]grantSelection, error) {
		gotOffered, gotKnown = candidates, known
		return []grantSelection{{handle: known[0], role: "admin"}}, nil
	}
	t.Cleanup(func() { grantPicker = prev })

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "github:alice")
	require.NoError(t, err)
	require.Equal(t, []string{"github:alice"}, gotKnown, "the typed grantee is the whole selection")
	require.Empty(t, gotOffered, "no pool is offered when the grantee was named")
	require.Contains(t, out, "✓ Granted github:alice admin access to project "+pickerProjULID)
}

// TestRemovePicker_DropsMembersTheRoutesCannotAddress: the remove pool applies
// the same filter as the add pool, and for a sharper reason. Revoking walks the
// chosen set and returns on the first error, so offering a member with no
// resolvable provider identity does not merely fail for that row — it strands
// every row after it.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_DropsMembersTheRoutesCannotAddress(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		inactive("github:pending", "acct-p"),
		member("github:alice", "acct-a"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	offered := captureRemovePicker(t, func(cs []grantCandidate) []grantCandidate { return []grantCandidate{cs[0]} })

	out, _, err := runCoreCmd(t, newOrgGrantCmd, srv.URL, "remove", pickerOrgULID)
	require.NoError(t, err)
	require.Equal(t, []string{"github:alice"}, handles(*offered), "an invited member is never offered")
	require.Contains(t, out, "✓ Revoked github:alice from org "+pickerOrgULID)
}

// TestRemovePicker_RowsCarryTheRole: revoking is destructive and the row is the
// last thing read before confirming, so it says what is being taken away and
// not only from whom. label stays the identity alone, because every message
// about the grant is built from it.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_RowsCarryTheRole(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{held: []holder{acctAlice}}, &grants, nil)
	t.Cleanup(srv.Close)
	offered := captureRemovePicker(t, func(cs []grantCandidate) []grantCandidate { return []grantCandidate{cs[0]} })

	_, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", pickerProjULID)
	require.NoError(t, err)
	require.Equal(t, []string{"github:alice"}, labels(*offered))
	require.Equal(t, []string{"github:alice (writer)"}, []string{(*offered)[0].option()})
}

// TestGrantRemove_ATypedGranteeIsNeverPrompted is the rule that keeps a
// scripted revoke working: the confirmation belongs to the picker, and a typed
// grantee already names exactly who to revoke.
//
// ENTIRE_TEST_TTY=1 is the point — a terminal IS available here, and the
// command still must not ask. Without this, `grant remove <ref> <handle>` with
// stdin redirected reached a form, read EOF, took the default of "no", and
// exited 0 having revoked nothing.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_ATypedGranteeIsNeverPrompted(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	asked := false
	prev := revokeConfirmed
	revokeConfirmed = func(*cobra.Command, grantPickerTarget, []grantCandidate) (bool, error) {
		asked = true
		return false, nil
	}
	t.Cleanup(func() { revokeConfirmed = prev })

	var revoked string
	srv := httptest.NewServer(grantWiringHandler(t,
		func(_, path string) { revoked = path },
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
	))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "remove", wiringProjULID, "github:alice")
	require.NoError(t, err)
	require.False(t, asked, "a named grantee is an instruction, not a proposal")
	require.Contains(t, revoked, "/grants/account/github/12345")
	require.Contains(t, out, "✓ Revoked github:alice")
}

// endlessOrgMembersServer pages org memberships forever, so only the fetch
// budget can end the walk. row builds the member at each position, which is
// what tells the truncation cases apart: grantable rows fill the pool, and
// ungrantable ones leave it empty while the org plainly has more.
func endlessOrgMembersServer(t *testing.T, row func(page, i int) coreapi.Membership) (*httptest.Server, *int) {
	t.Helper()
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/members") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return
		}
		pages++
		members := make([]coreapi.Membership, budgetTestPageSize)
		for i := range members {
			members[i] = row(pages, i)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := printJSON(w, &coreapi.ListOrgMembersOutputBody{
			Members:       members,
			NextPageToken: coreapi.NewOptString(fmt.Sprintf("page-%d", pages+1)),
		}); err != nil {
			t.Errorf("encode members: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pages
}

// budgetTestPageSize is chosen so two pages clear coreListFetchBudget and a
// third is never asked for.
const budgetTestPageSize = 600

// TestRemovePicker_APoolLongerThanTheBudgetIsDisclosed: a pool is read in full
// before a single row can be shown, so it stops at the shared fetch budget
// rather than walking an org of any size one round trip per page. A window onto
// a long list is still useful — looking complete is what it must not do, so the
// truncation is disclosed on stderr along with the way past it.
//
// The caveat travels ON the picker, not to stderr: the form may be rendering on
// the controlling terminal precisely because stderr is redirected, and a
// warning sent to a stream nobody is reading is missing in exactly the case it
// exists for.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_APoolLongerThanTheBudgetIsDisclosed(t *testing.T) {
	srv, requested := endlessOrgMembersServer(t, func(page, i int) coreapi.Membership {
		return member(fmt.Sprintf("github:u%d-%d", page, i), fmt.Sprintf("acct-%d-%d", page, i))
	})

	t.Setenv("ENTIRE_TEST_TTY", "1")
	var offered []grantCandidate
	var shown grantPickerTarget
	prev := removePicker
	removePicker = func(_ *cobra.Command, pt grantPickerTarget, candidates []grantCandidate) ([]grantCandidate, error) {
		offered, shown = candidates, pt
		return nil, nil
	}
	t.Cleanup(func() { removePicker = prev })

	_, stderr, err := runCoreCmd(t, newOrgGrantCmd, srv.URL, "remove", pickerOrgULID)
	require.NoError(t, err, "choosing nobody is a clean stop")
	require.Equal(t, 2, *requested, "the walk stops at the budget, not at the end of an endless list")
	require.Len(t, offered, 2*budgetTestPageSize)
	// 1200 is what was READ, not what survived filtering: the count only ever
	// says how much of the listing the pool covers.
	require.Contains(t, shown.poolNote, "Only the first 1200 grants on org "+pickerOrgULID+" were read")
	require.Contains(t, shown.poolNote, "name the grantee")
	require.NotContains(t, stderr, "Only the first", "the caveat belongs on the screen the rows are on")
}

// TestGrantPicker_ATruncatedPoolSaysSoOnTheScreen is the add side of the same
// rule. Only the first page's members hold grants, so the second page fills the
// pool and the picker opens with a window onto a longer org.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_ATruncatedPoolSaysSoOnTheScreen(t *testing.T) {
	held := make([]coreapi.ProjectGrant, 0, budgetTestPageSize)
	for i := range budgetTestPageSize {
		held = append(held, coreapi.ProjectGrant{
			GranteeId:   fmt.Sprintf("acct-1-%d", i),
			GranteeType: granteeTypeAccount,
			GranteeName: coreapi.NewOptString(fmt.Sprintf("github:u1-%d", i)),
			Role:        "writer",
			Source:      grantSourceDirect,
		})
	}
	srv := truncatedAddPoolServer(t, held)

	t.Setenv("ENTIRE_TEST_TTY", "1")
	var shown grantPickerTarget
	prev := grantPicker
	grantPicker = func(_ *cobra.Command, pt grantPickerTarget, _ []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		shown = pt
		return nil, nil
	}
	t.Cleanup(func() { grantPicker = prev })

	_, stderr, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "add", pickerProjULID)
	require.NoError(t, err)
	require.Contains(t, shown.poolNote, "Only the first 1200 members of the org owning project "+pickerProjULID+" were read")
	require.NotContains(t, stderr, "Only the first", "the caveat belongs on the screen the rows are on")
}

// TestRemovePicker_ATruncatedEmptyPoolSaysSo: "has no grants that can be
// revoked here" is a statement about the target, and a truncated walk only read
// the first N rows on it. Reporting the unqualified sentence would tell a user
// their grant does not exist when it was simply never looked at.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_ATruncatedEmptyPoolSaysSo(t *testing.T) {
	// Nobody addressable, so the window is full and the pool empty.
	srv, _ := endlessOrgMembersServer(t, func(page, i int) coreapi.Membership {
		return inactive(fmt.Sprintf("github:u%d-%d", page, i), fmt.Sprintf("acct-%d-%d", page, i))
	})
	captureRemovePicker(t, func([]grantCandidate) []grantCandidate {
		t.Error("the picker must not open with nothing to offer")
		return nil
	})

	_, _, err := runCoreCmd(t, newOrgGrantCmd, srv.URL, "remove", pickerOrgULID)
	require.ErrorContains(t, err, "none of the first 1200 grants on org "+pickerOrgULID+" can be revoked here, and it has more")
	require.ErrorContains(t, err, "entire org grant remove "+pickerOrgULID+" github:alice")
	require.NotContains(t, err.Error(), "has no grants that can be revoked here",
		"the unqualified sentence claims the target has none, which this never established")
}

// TestGrantPicker_ATruncatedEmptyPoolIsNotAQuietSuccess: the three empty-pool
// messages are each a statement about the WHOLE org, so a truncated walk can
// say none of them. Exiting 0 with "every member already has a grant" would
// report a state this never looked for past the budget, so the truncated case
// is an error naming the explicit form instead.
//
// The grant listing is finite and covers exactly the members the budget takes,
// so the window comes back empty while the org plainly has more.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_ATruncatedEmptyPoolIsNotAQuietSuccess(t *testing.T) {
	held := make([]coreapi.ProjectGrant, 0, 2*budgetTestPageSize)
	for page := 1; page <= 2; page++ {
		for i := range budgetTestPageSize {
			held = append(held, coreapi.ProjectGrant{
				GranteeId:   fmt.Sprintf("acct-%d-%d", page, i),
				GranteeType: granteeTypeAccount,
				GranteeName: coreapi.NewOptString(fmt.Sprintf("github:u%d-%d", page, i)),
				Role:        "writer",
				Source:      grantSourceDirect,
			})
		}
	}
	srv := truncatedAddPoolServer(t, held)

	capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
		t.Error("the picker must not open with nothing to offer")
		return nil, nil
	})

	_, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL, "add", pickerProjULID)
	require.ErrorContains(t, err, "none of the first 1200 members of the org owning project "+pickerProjULID+" can be added here, and it has more")
	require.ErrorContains(t, err, "github:alice --role reader")
	require.NotContains(t, err.Error(), "already has a grant",
		"that sentence is about the whole org, which this never read")
}

// TestCancelledPicker_NamesWhatWasCancelled: the grantee multi-select is the
// same screen for both flows, so backing out of `grant remove`'s picker once
// read "Grant cancelled." The action is the caller's to name, and revoking says
// the same word here as the confirmation one screen later does.
func TestCancelledPicker_NamesWhatWasCancelled(t *testing.T) {
	t.Parallel()
	for action, want := range map[string]string{
		grantAction:  "Grant cancelled.",
		revokeAction: "Revocation cancelled.",
	} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			var render bytes.Buffer
			err := cancelledPicker(&render, action, huh.ErrUserAborted)
			require.Error(t, err, "the command still stops")
			require.Equal(t, want+"\n", render.String())
		})
	}
}

// truncatedAddPoolServer pages the owning org's membership forever while
// serving held as the project's grants. The membership is the pool, so the
// budget truncates it; the grants are the filter, which stays unbounded and so
// has to be finite or the walk over it never ends.
func truncatedAddPoolServer(t *testing.T, held []coreapi.ProjectGrant) *httptest.Server {
	t.Helper()
	memberPages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var payload any
		switch {
		case strings.HasSuffix(r.URL.Path, "/members") && strings.Contains(r.URL.Path, "/orgs/"):
			memberPages++
			members := make([]coreapi.Membership, budgetTestPageSize)
			for i := range members {
				members[i] = member(fmt.Sprintf("github:u%d-%d", memberPages, i), fmt.Sprintf("acct-%d-%d", memberPages, i))
			}
			payload = &coreapi.ListOrgMembersOutputBody{
				Members:       members,
				NextPageToken: coreapi.NewOptString(fmt.Sprintf("page-%d", memberPages+1)),
			}
		case strings.HasSuffix(r.URL.Path, "/members"):
			payload = &coreapi.ListProjectMembersOutputBody{Members: held}
		case strings.HasSuffix(r.URL.Path, "/projects/"+pickerProjULID):
			payload = &coreapi.Project{ID: pickerProjULID, Name: "widgets", OwnerId: pickerOrgULID, OwnerType: coreapi.ProjectOwnerTypeOrg}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		if err := printJSON(w, payload); err != nil {
			t.Errorf("encode %s: %v", r.URL.Path, err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRevokeConfirmed_ACancelledContextIsNotADecline: (false, nil) is reserved
// for the user answering no, because the caller exits 0 on it. A context
// cancelled out from under the gate is an interruption, and reporting it as a
// decline exited 0 having revoked nothing and said nothing — the silent no-op
// this command's confirmation exists to avoid.
//
// Wrapping ctx.Err() is also load-bearing: main.go matches on
// errors.Is(err, context.Canceled) plus the signal it recorded to re-raise it,
// which is what gives Ctrl+C its usual quiet 130 and breaks an enclosing shell
// loop. The same helper guards the far side of the form, where a signal can
// land while it is up.
func TestRevokeConfirmed_ACancelledContextIsNotADecline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var render bytes.Buffer
	cmd.SetErr(&render)

	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}
	proceed, err := revokeConfirmed(cmd, pt, []grantCandidate{{ref: "x", label: "github:alice"}})

	require.False(t, proceed, "nothing was confirmed")
	require.ErrorIs(t, err, context.Canceled, "main.go keys the quiet signal exit off this")
	require.Empty(t, render.String(), "an interruption is not the user being told they declined")
}
