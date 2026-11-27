package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const teamRepoRef = "/et/widgets/web"

func runGrantCmd(t *testing.T, newCmd func() *cobra.Command, srv *httptest.Server, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCoreCmd(t, newCmd, srv.URL, args...)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_TeamGranteeSendsTheTeamForm(t *testing.T) {
	cases := []struct {
		noun     string
		newCmd   func() *cobra.Command
		ref      string
		wantPath string
	}{
		{"project", newProjectGrantCmd, teamProjULID, "/api/v1/projects/" + teamProjULID + "/grants"},
		{"repo", newRepoGrantCmd, teamRepoRef, "/api/v1/repos/" + teamRepoULID + "/grants"},
	}
	for _, tc := range cases {
		t.Run(tc.noun, func(t *testing.T) {
			f := newTeamCore()
			srv := f.serve(t)

			out, _, err := runGrantCmd(t, tc.newCmd, srv, "add", tc.ref, "team:platform", "--role", "writer")
			require.NoError(t, err)
			require.Len(t, f.writes, 1)
			assert.Equal(t, "POST "+tc.wantPath+` {"granteeType":"team","role":"writer","team_id":"`+teamPlatformULID+`"}`, f.writes[0])
			assert.Contains(t, out, "✓ Granted team:platform writer access to "+tc.noun+" ")
		})
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_TeamGranteeJSON(t *testing.T) {
	srv := newTeamCore().serve(t)

	out, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "add", teamProjULID, "team:platform", "--role", "reader", "--json")
	require.NoError(t, err)
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	assert.Equal(t, "granted", body["status"])
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_TeamGranteeRefusals(t *testing.T) {
	t.Run("unknown team", func(t *testing.T) {
		f := newTeamCore()
		srv := f.serve(t)
		_, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "add", teamProjULID, "team:nope", "--role", "reader")
		require.ErrorContains(t, err, `no team named "nope" in the org that owns project `+teamProjULID)
		require.ErrorContains(t, err, "run `entire org team list "+testOrgULID+"`")
		assert.Empty(t, f.writes)
	})
	// The prefix is exact, so Team:platform is a handle on a provider called
	// Team. The failed lookup lists the forms a grantee takes.
	t.Run("wrong-case team prefix", func(t *testing.T) {
		f := newTeamCore()
		f.noIdentity = true
		srv := f.serve(t)
		_, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "add", teamProjULID, "Team:platform", "--role", "reader")
		require.ErrorContains(t, err, `no Team identity for handle "platform"; a grantee is team:<name> or a handle like github:alice`)
		assert.Empty(t, f.writes)
	})
	t.Run("account-owned project", func(t *testing.T) {
		f := newTeamCore()
		f.ownerType = coreapi.ProjectOwnerTypeAccount
		srv := f.serve(t)
		_, _, err := runGrantCmd(t, newRepoGrantCmd, srv, "add", teamRepoRef, "team:platform", "--role", "reader")
		require.ErrorContains(t, err, "project widgets is owned by an account, not an org; only an org's projects and repos take team grants")
		assert.Empty(t, f.writes)
	})
	t.Run("core 404", func(t *testing.T) {
		f := newTeamCore()
		f.writeStatus = http.StatusNotFound
		srv := f.serve(t)
		_, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "add", teamProjULID, "team:platform", "--role", "reader")
		// main.go prints renderCoreError's text, which drops everything but
		// Core's message when the error wraps a Core error.
		rendered := renderCoreError(err).Error()
		require.Contains(t, rendered, "team platform is not a team of the org that owns project "+teamProjULID)
		require.Contains(t, rendered, "refused by core")
	})
	// The CLI refused an account-owned target above, so a 422 is some other
	// refusal and keeps Core's own message.
	t.Run("core 422", func(t *testing.T) {
		f := newTeamCore()
		f.writeStatus = http.StatusUnprocessableEntity
		srv := f.serve(t)
		_, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "add", teamProjULID, "team:platform", "--role", "reader")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "is not owned by an org")
	})
}

// An org takes no team grantee, so the refusal costs no request.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgGrant_RefusesATeamGrantee(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for _, verb := range []string{"add", "remove"} {
		_, _, err := runGrantCmd(t, newOrgGrantCmd, srv, verb, testOrgULID, "team:platform")
		require.ErrorContains(t, err, "team:platform is a team; a team cannot hold org access")
		_, _, err = runGrantCmd(t, newOrgGrantCmd, srv, verb, testOrgULID, "team:")
		require.ErrorContains(t, err, "team: is a team; a team cannot hold org access")
		_, _, err = runGrantCmd(t, newProjectGrantCmd, srv, verb, teamProjULID, "team:")
		require.ErrorContains(t, err, "team: names no team; write team:<name>")
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantRemove_TeamGranteeRevokesByTeamID(t *testing.T) {
	cases := []struct {
		noun     string
		newCmd   func() *cobra.Command
		ref      string
		wantPath string
	}{
		{"project", newProjectGrantCmd, teamProjULID, "/api/v1/projects/" + teamProjULID + "/grants/team/" + teamPlatformULID},
		{"repo", newRepoGrantCmd, teamRepoRef, "/api/v1/repos/" + teamRepoULID + "/grants/team/" + teamPlatformULID},
	}
	for _, tc := range cases {
		t.Run(tc.noun, func(t *testing.T) {
			f := newTeamCore()
			srv := f.serve(t)

			out, _, err := runGrantCmd(t, tc.newCmd, srv, "remove", tc.ref, "team:platform")
			require.NoError(t, err)
			assert.Equal(t, []string{"DELETE " + tc.wantPath}, f.writes)
			assert.Contains(t, out, "✓ Revoked team:platform from "+tc.noun+" ")
		})
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantList_NamesTeamGrantees(t *testing.T) {
	f := newTeamCore()
	f.projGrants = []coreapi.ProjectGrant{
		{GranteeId: teamPlatformULID, GranteeType: granteeTypeTeam, GranteeName: coreapi.NewOptString("platform"), Role: "writer", Source: grantSourceDirect},
		{GranteeId: teamDirULID, GranteeType: granteeTypeTeam, Role: "reader", Source: grantSourceDirect},
	}
	srv := f.serve(t)

	out, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "list", teamProjULID)
	require.NoError(t, err)
	assert.Regexp(t, `team:platform\s+-\s+writer\s+direct\s+team`, out)
	assert.Regexp(t, teamDirULID+`\s+-\s+reader\s+direct\s+team`, out, "a team Core could not name falls back to its ULID")
}

// The remove picker offers a direct team grant and revokes it as a team.
//
// Not parallel: swaps the activeCoreClient and removePicker seams.
func TestRemovePicker_OffersTeamGrants(t *testing.T) {
	f := newTeamCore()
	f.projGrants = []coreapi.ProjectGrant{
		{GranteeId: teamPlatformULID, GranteeType: granteeTypeTeam, GranteeName: coreapi.NewOptString("platform"), Role: "writer", Source: grantSourceDirect},
		{GranteeId: testOrgULID, GranteeType: "org", GranteeName: coreapi.NewOptString("acme"), Role: "owner", Source: "owner"},
	}
	srv := f.serve(t)
	offered := captureRemovePicker(t, func(cs []grantCandidate) []grantCandidate { return cs })

	out, _, err := runGrantCmd(t, newProjectGrantCmd, srv, "remove", teamProjULID)
	require.NoError(t, err)
	assert.Equal(t, []string{"team:platform"}, labels(*offered))
	assert.Equal(t, []string{"DELETE /api/v1/projects/" + teamProjULID + "/grants/team/" + teamPlatformULID}, f.writes)
	assert.Contains(t, out, "✓ Revoked team:platform from project "+teamProjULID)
}

func TestGrantRowName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "team:platform", grantRowName(granteeTypeTeam, "platform", teamPlatformULID))
	assert.Equal(t, teamPlatformULID, grantRowName(granteeTypeTeam, "", teamPlatformULID))
	assert.Equal(t, "github:alice", grantRowName(granteeTypeAccount, "github:alice", acctAlice.id))
}

func TestTeamGranteeName(t *testing.T) {
	t.Parallel()
	name, ok := teamGranteeName("team:platform")
	assert.True(t, ok)
	assert.Equal(t, "platform", name)
	_, ok = teamGranteeName("Team:platform")
	assert.False(t, ok, "the prefix is exact: Team:platform is a handle")
	name, ok = teamGranteeName("team:")
	assert.True(t, ok, "an empty name is still a team, so its refusal can say so")
	assert.Empty(t, name)
	_, ok = teamGranteeName("github:alice")
	assert.False(t, ok)
	_, ok = teamGranteeName("tea")
	assert.False(t, ok)
}

// A team member is never a team, so every team: form gets the same refusal,
// as org grant gives.
func TestEnsureMemberIsHandle_RefusesEveryTeamForm(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"team:platform", "team:"} {
		require.ErrorContains(t, ensureMemberIsHandle(ref), ref+" is a team; a team member is a user like github:alice")
	}
}
