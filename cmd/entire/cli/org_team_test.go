package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	teamPlatformULID = "01HZX7QABCDEFGHJKMNPQRSTT1"
	teamDirULID      = "01HZX7QABCDEFGHJKMNPQRSTT2"
	teamProjULID     = "01HZX7QABCDEFGHJKMNPQRSTT3"
	teamRepoULID     = "01HZX7QABCDEFGHJKMNPQRSTT4"
)

func testTeam(id, name string, managed bool) coreapi.Team {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return coreapi.Team{ID: id, OrgID: testOrgULID, Name: name, DirectoryManaged: managed, CreatedAt: at, UpdatedAt: at}
}

// teamCore is a fake Core for the team and team-grant commands. It records
// every write as "METHOD path body" and answers it with writeStatus, plus a
// problem type when writeType is set.
type teamCore struct {
	teams       []coreapi.Team
	teamMembers []coreapi.TeamMember
	orgMembers  []coreapi.OrgMemberListItem
	ownerType   coreapi.ProjectOwnerType
	projGrants  []coreapi.ProjectGrant
	writeStatus int
	writeType   string
	writes      []string
}

func (f *teamCore) serve(t *testing.T) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, code int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := printJSON(w, payload); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	ownerType := f.ownerType
	if ownerType == "" {
		ownerType = coreapi.ProjectOwnerTypeOrg
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/repos/resolve") {
			var body coreapi.ResolveReposInputBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Repositories) == 0 {
				t.Errorf("decode resolve body: %v", err)
				return
			}
			write(w, http.StatusOK, nativeResolution(body.Repositories[0].FullName, teamRepoULID))
			return
		}
		if r.Method != http.MethodGet {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			f.writes = append(f.writes, strings.TrimSpace(r.Method+" "+path+" "+string(body)))
			code := f.writeStatus
			if code >= http.StatusBadRequest {
				problem := map[string]any{"status": code, "detail": "refused by core"}
				if f.writeType != "" {
					problem["type"] = f.writeType
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(code)
				if err := json.NewEncoder(w).Encode(problem); err != nil {
					t.Errorf("write problem: %v", err)
				}
				return
			}
			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/teams"):
				var in coreapi.CreateTeamInputBody
				if err := json.Unmarshal(body, &in); err != nil {
					t.Errorf("decode create body: %v", err)
				}
				team := testTeam(teamPlatformULID, in.Name, false)
				write(w, http.StatusCreated, &team)
			case r.Method == http.MethodPatch:
				var in coreapi.UpdateTeamInputBody
				if err := json.Unmarshal(body, &in); err != nil {
					t.Errorf("decode update body: %v", err)
				}
				team := testTeam(teamPlatformULID, in.Name.Or(""), false)
				write(w, http.StatusOK, &team)
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/grants"):
				write(w, http.StatusCreated, map[string]string{"status": "granted"})
			default:
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		switch {
		case strings.Contains(path, "/identity/handles/"):
			seg := strings.Split(strings.Trim(path, "/"), "/")
			handle := seg[len(seg)-1]
			write(w, http.StatusOK, &coreapi.ResolvedIdentity{
				AccountId: "acct-" + handle, Provider: providerGitHub, Handle: handle, ProviderUserId: "uid-" + handle,
			})
		case strings.HasSuffix(path, "/teams"):
			write(w, http.StatusOK, &coreapi.ListTeamsOutputBody{Teams: f.teams})
		case strings.Contains(path, "/teams/") && strings.HasSuffix(path, "/members"):
			write(w, http.StatusOK, &coreapi.ListTeamMembersOutputBody{Members: f.teamMembers})
		case strings.Contains(path, "/orgs/") && strings.HasSuffix(path, "/members"):
			write(w, http.StatusOK, &coreapi.ListOrgMembersOutputBody{Members: f.orgMembers})
		case strings.HasSuffix(path, "/projects/"+teamProjULID+"/members"):
			write(w, http.StatusOK, &coreapi.ListProjectMembersOutputBody{Members: f.projGrants})
		case strings.HasSuffix(path, "/projects"):
			write(w, http.StatusOK, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
				ID: teamProjULID, Name: "widgets", OwnerId: testOrgULID, OwnerType: ownerType,
			})})
		case strings.HasSuffix(path, "/repos"):
			write(w, http.StatusOK, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
				ID: teamRepoULID, Name: "web", OwningProjectId: teamProjULID,
			})})
		case strings.HasSuffix(path, "/repos/"+teamRepoULID):
			write(w, http.StatusOK, &coreapi.Repo{ID: teamRepoULID, Name: "web", OwningProjectId: teamProjULID})
		case strings.HasSuffix(path, "/projects/"+teamProjULID):
			write(w, http.StatusOK, &coreapi.Project{ID: teamProjULID, Name: "widgets", OwnerId: testOrgULID, OwnerType: ownerType})
		default:
			t.Errorf("unexpected GET %s", path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTeamCore() *teamCore {
	return &teamCore{teams: []coreapi.Team{
		testTeam(teamPlatformULID, "platform", false),
		testTeam(teamDirULID, "eng", true),
	}}
}

func runOrgCmd(t *testing.T, srv *httptest.Server, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCoreCmd(t, newOrgCmd, srv.URL, args...)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamList_MarksDirectoryManagedTeams(t *testing.T) {
	srv := newTeamCore().serve(t)

	out, _, err := runOrgCmd(t, srv, "team", "list", testOrgULID)
	require.NoError(t, err)
	assert.Contains(t, out, "MANAGED BY")
	assert.Regexp(t, `platform\s+-\s+2026-01-01`, out)
	assert.Regexp(t, `eng\s+directory\s+2026-01-01`, out)

	out, _, err = runOrgCmd(t, srv, "team", "list", testOrgULID, "--json")
	require.NoError(t, err)
	var teams []coreapi.Team
	require.NoError(t, json.Unmarshal([]byte(out), &teams))
	require.Len(t, teams, 2)
	assert.Equal(t, teamPlatformULID, teams[0].ID)
	assert.True(t, teams[1].DirectoryManaged)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamCreate_SendsTheName(t *testing.T) {
	f := newTeamCore()
	srv := f.serve(t)

	out, _, err := runOrgCmd(t, srv, "team", "create", testOrgULID, "infra")
	require.NoError(t, err)
	require.Len(t, f.writes, 1)
	assert.Equal(t, "POST /api/v1/orgs/"+testOrgULID+`/teams {"name":"infra"}`, f.writes[0])
	assert.Contains(t, out, "✓ Created team infra in org "+testOrgULID)

	out, _, err = runOrgCmd(t, srv, "team", "create", testOrgULID, "infra", "--json")
	require.NoError(t, err)
	var team coreapi.Team
	require.NoError(t, json.Unmarshal([]byte(out), &team))
	assert.Equal(t, "infra", team.Name)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeam_MapsCoreConflicts(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		probType string
		want     string
	}{
		{"create name in use", []string{"create", testOrgULID, "platform"}, problemTypeTeamNameInUse, "org " + testOrgULID + " already has a team named platform"},
		{"rename name in use", []string{"rename", testOrgULID, "platform", "eng"}, problemTypeTeamNameInUse, "already has a team named eng"},
		{"rename directory team", []string{"rename", testOrgULID, "eng", "core"}, problemTypeTeamDirectoryManaged, "managed by your directory"},
		{"delete directory team", []string{"delete", testOrgULID, "eng", "--force"}, problemTypeTeamDirectoryManaged, "team eng is managed by your directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTeamCore()
			f.writeStatus, f.writeType = http.StatusConflict, tc.probType
			srv := f.serve(t)

			_, _, err := runOrgCmd(t, srv, append([]string{"team"}, tc.args...)...)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamRename_ResolvesTheTeamByName(t *testing.T) {
	f := newTeamCore()
	srv := f.serve(t)

	out, _, err := runOrgCmd(t, srv, "team", "rename", testOrgULID, "Platform", "infra")
	require.NoError(t, err)
	require.Equal(t, []string{"PATCH /api/v1/orgs/" + testOrgULID + "/teams/" + teamPlatformULID + ` {"name":"infra"}`}, f.writes)
	assert.Contains(t, out, "✓ Renamed team platform to infra in org "+testOrgULID)

	_, _, err = runOrgCmd(t, srv, "team", "rename", testOrgULID, "nope", "infra")
	require.ErrorContains(t, err, `no team named "nope" in org `+testOrgULID)
	assert.Len(t, f.writes, 1, "an unknown team must not reach a write")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamDelete_ConfirmsLikeOtherDeletes(t *testing.T) {
	f := newTeamCore()
	srv := f.serve(t)

	_, _, err := runOrgCmd(t, srv, "team", "delete", testOrgULID, "platform")
	require.ErrorContains(t, err, "refusing to delete")
	require.Empty(t, f.writes)

	out, _, err := runOrgCmd(t, srv, "team", "delete", testOrgULID, "platform", "--force")
	require.NoError(t, err)
	require.Equal(t, []string{"DELETE /api/v1/orgs/" + testOrgULID + "/teams/" + teamPlatformULID}, f.writes)
	assert.Contains(t, out, "✓ Deleted org "+testOrgULID+" team platform ("+teamPlatformULID+")")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamMemberList_ShowsStatusOnlyWhenNotActive(t *testing.T) {
	f := newTeamCore()
	f.teamMembers = []coreapi.TeamMember{
		{AccountID: acctAlice.id, Handle: coreapi.NewOptString("github:alice"), DisplayName: coreapi.NewOptString("Alice"), Status: "active"},
		{AccountID: acctBob.id, Handle: coreapi.NewOptString("github:bob"), Status: "pending"},
		{AccountID: "01HZX7QABCDEFGHJKMNPQRSTC3", Username: coreapi.NewOptString("carol"), Status: "inactive"},
	}
	srv := f.serve(t)

	out, _, err := runOrgCmd(t, srv, "team", "member", "list", testOrgULID, "platform")
	require.NoError(t, err)
	assert.Regexp(t, `github:alice\s+Alice\s+-\n`, out)
	assert.Regexp(t, `github:bob\s+-\s+pending\n`, out)
	assert.Regexp(t, `carol\s+-\s+inactive\n`, out)

	out, _, err = runOrgCmd(t, srv, "team", "member", "list", testOrgULID, "platform", "--json")
	require.NoError(t, err)
	var members []coreapi.TeamMember
	require.NoError(t, json.Unmarshal([]byte(out), &members))
	require.Len(t, members, 3)
	assert.Equal(t, "active", members[0].Status)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamMemberAdd_ResolvesTheHandle(t *testing.T) {
	f := newTeamCore()
	srv := f.serve(t)

	out, _, err := runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform", "github:alice")
	require.NoError(t, err)
	require.Equal(t, []string{"PUT /api/v1/orgs/" + testOrgULID + "/teams/" + teamPlatformULID + "/members/acct-alice"}, f.writes)
	assert.Contains(t, out, "✓ Added github:alice to team platform")

	out, _, err = runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform", "github:alice", "--json")
	require.NoError(t, err)
	var change teamMemberChange
	require.NoError(t, json.Unmarshal([]byte(out), &change))
	assert.Equal(t, teamMemberChange{OrgID: testOrgULID, TeamID: teamPlatformULID, AccountID: "acct-alice", Member: "github:alice"}, change)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamMemberAdd_MapsCoreRefusals(t *testing.T) {
	cases := []struct {
		probType string
		want     string
	}{
		{problemTypeInvitationRequired, "github:alice is not an active member of org " + testOrgULID + "; invite them to the org first: entire org invite send " + testOrgULID + " --account github:alice"},
		{problemTypeTeamDirectoryManaged, "team platform is managed by your directory"},
	}
	for _, tc := range cases {
		t.Run(tc.probType, func(t *testing.T) {
			f := newTeamCore()
			f.writeStatus, f.writeType = http.StatusConflict, tc.probType
			srv := f.serve(t)

			_, _, err := runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform", "github:alice")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamMemberAdd_RefusesBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform")
	require.ErrorContains(t, err, "no user given; pass one as provider:handle")

	_, _, err = runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform", "team:eng")
	require.ErrorContains(t, err, "team:eng is a team")

	_, _, err = runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform", acctAlice.id)
	require.ErrorContains(t, err, "is an account ULID")
}

// The picker offers active org members not already on the team.
//
// Not parallel: swaps the activeCoreClient and teamMemberPicker seams.
func TestOrgTeamMemberAdd_PickerOffersActiveNonMembers(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	f := newTeamCore()
	f.orgMembers = []coreapi.OrgMemberListItem{
		member("github:alice", acctAlice.id),
		member("github:bob", acctBob.id),
		inactive("github:carol", "01HZX7QABCDEFGHJKMNPQRSTC3"),
	}
	f.teamMembers = []coreapi.TeamMember{{AccountID: acctAlice.id, Status: "active"}}
	srv := f.serve(t)

	var offered []grantCandidate
	prev := teamMemberPicker
	teamMemberPicker = func(_ *cobra.Command, _ grantPickerTarget, candidates []grantCandidate) ([]grantCandidate, error) {
		offered = candidates
		return candidates, nil
	}
	t.Cleanup(func() { teamMemberPicker = prev })

	out, _, err := runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform")
	require.NoError(t, err)
	assert.Equal(t, []string{"github:bob"}, labels(offered))
	assert.Equal(t, []string{"PUT /api/v1/orgs/" + testOrgULID + "/teams/" + teamPlatformULID + "/members/" + acctBob.id}, f.writes)
	assert.Contains(t, out, "✓ Added github:bob to team platform")
}

// Not parallel: swaps the activeCoreClient and teamMemberPicker seams.
func TestOrgTeamMemberAdd_EmptyPoolAddsNobody(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	f := newTeamCore()
	f.orgMembers = []coreapi.OrgMemberListItem{member("github:alice", acctAlice.id)}
	f.teamMembers = []coreapi.TeamMember{{AccountID: acctAlice.id, Status: "active"}}
	srv := f.serve(t)

	prev := teamMemberPicker
	teamMemberPicker = func(*cobra.Command, grantPickerTarget, []grantCandidate) ([]grantCandidate, error) {
		t.Error("an empty pool must not open the picker")
		return nil, nil
	}
	t.Cleanup(func() { teamMemberPicker = prev })

	out, _, err := runOrgCmd(t, srv, "team", "member", "add", testOrgULID, "platform")
	require.NoError(t, err)
	assert.Contains(t, out, "every active member of org "+testOrgULID+" is already on team platform")
	assert.Empty(t, f.writes)
}

// Not parallel: swaps the activeCoreClient and teamMemberRemoveConfirmed seams.
func TestOrgTeamMemberRemove_ConfirmsOnATerminal(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	f := newTeamCore()
	srv := f.serve(t)

	var asked []string
	answer := false
	prev := teamMemberRemoveConfirmed
	teamMemberRemoveConfirmed = func(_ *cobra.Command, member, team string) (bool, error) {
		asked = append(asked, member+" "+team)
		return answer, nil
	}
	t.Cleanup(func() { teamMemberRemoveConfirmed = prev })

	_, _, err := runOrgCmd(t, srv, "team", "member", "remove", testOrgULID, "platform", "github:alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"github:alice platform"}, asked)
	assert.Empty(t, f.writes, "a declined prompt removes nobody")

	answer = true
	out, _, err := runOrgCmd(t, srv, "team", "member", "remove", testOrgULID, "platform", "github:alice")
	require.NoError(t, err)
	want := "DELETE /api/v1/orgs/" + testOrgULID + "/teams/" + teamPlatformULID + "/members/acct-alice"
	assert.Equal(t, []string{want}, f.writes)
	assert.Contains(t, out, "✓ Removed github:alice from team platform")

	_, _, err = runOrgCmd(t, srv, "team", "member", "remove", testOrgULID, "platform", "github:alice", "--yes")
	require.NoError(t, err)
	assert.Len(t, asked, 2, "--yes skips the prompt")
	assert.Equal(t, []string{want, want}, f.writes)
}

// Without a terminal the typed user is the whole instruction.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgTeamMemberRemove_NoPromptWithoutATerminal(t *testing.T) {
	f := newTeamCore()
	srv := f.serve(t)

	_, _, err := runOrgCmd(t, srv, "team", "member", "remove", testOrgULID, "platform", "github:alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"DELETE /api/v1/orgs/" + testOrgULID + "/teams/" + teamPlatformULID + "/members/acct-alice"}, f.writes)
}
