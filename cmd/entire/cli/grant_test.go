package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// TestValidateChoice covers the check every `<noun> grant add` runs on --role:
// the value matches one of the target's roles exactly (the server enums are
// lowercase) and the message lists what would have been accepted. The same
// function checks `org invite list --status`, so it names the flag it rejected.
func TestValidateChoice(t *testing.T) {
	t.Parallel()
	roles := []string{"reader", "writer", "admin"}
	for _, ok := range roles {
		require.NoError(t, validateChoice("role", ok, roles))
	}
	for _, bad := range []string{"", "owner", "Reader", "member"} {
		require.ErrorContains(t, validateChoice("role", bad, roles), "invalid --role "+strconv.Quote(bad)+": must be one of reader, writer, admin")
	}
	require.ErrorContains(t, validateChoice("status", "pending", invitationStatuses), "invalid --status "+strconv.Quote("pending")+": must be one of open, accepted, revoked, expired, all")
}

// TestGrantTargetRoles pins each target's role set and default: org
// membership has owner/admin/member with member as the server default, while
// project and repo access has reader/writer/admin and no default, so --role is
// required there.
//
// Each list is also checked against the generated client's enum for that
// target's grant body. The lists are bare strings cast into those enum types,
// so nothing else notices when a regenerated client renames, drops or adds a
// role: the help would advertise a role the server refuses, or refuse one it
// accepts. The deleted per-target switch used to catch that at compile time;
// this is the same guard as a test.
func TestGrantTargetRoles(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"owner", "admin", "member"}, orgGrantTarget.roles)
	require.Equal(t, "member", orgGrantTarget.defaultRole)
	require.Equal(t, []string{"reader", "writer", "admin"}, projectGrantTarget.roles)
	require.Empty(t, projectGrantTarget.defaultRole)
	require.Equal(t, []string{"reader", "writer", "admin"}, repoGrantTarget.roles)
	require.Empty(t, repoGrantTarget.defaultRole)

	require.Equal(t, enumStrings(coreapi.AddOrgMemberInputBodyRole("").AllValues()), orgGrantTarget.roles)
	require.Equal(t, enumStrings(coreapi.GrantAccessBodyRole("").AllValues()), projectGrantTarget.roles)
	require.Equal(t, enumStrings(coreapi.GrantAccessBodyRole("").AllValues()), repoGrantTarget.roles)
}

// enumStrings converts a generated enum's AllValues() into the plain strings a
// grantTarget lists.
func enumStrings[E ~string](values []E) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func TestGranteeName(t *testing.T) {
	t.Parallel()
	const ulid = "01HZX0000000000000000000AB"
	tests := []struct {
		name string
		in   coreapi.OptString
		id   string
		want string
	}{
		{name: "friendly name wins", in: coreapi.NewOptString("github:alice"), id: ulid, want: "github:alice"},
		{name: "google minted handle shows the subject id", in: coreapi.NewOptString("google:google-1001"), id: ulid, want: "google:1001"},
		{name: "unset falls back to ULID", in: coreapi.OptString{}, id: ulid, want: ulid},
		{name: "empty string falls back to ULID", in: coreapi.NewOptString(""), id: ulid, want: ulid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := granteeName(tt.in, tt.id); got != tt.want {
				t.Errorf("granteeName(%v, %q) = %q, want %q", tt.in, tt.id, got, tt.want)
			}
		})
	}
}

func TestGrantRows(t *testing.T) {
	t.Parallel()
	const ulid = "01HZX0000000000000000000AB"

	// grantColumns and the row builders must stay in lockstep — same width,
	// same column order — or the table header and cells misalign. No column
	// carries an internal id: the grantee ULID stays in --json only.
	require.Equal(t, []string{"GRANTEE", "ROLE", "SOURCE", "TYPE"}, grantColumns)

	t.Run("project resolved name", func(t *testing.T) {
		t.Parallel()
		row := projectGrantRow(coreapi.ProjectGrant{
			GranteeId:   ulid,
			GranteeName: coreapi.NewOptString("github:alice"),
			GranteeType: "account",
			Role:        "writer",
			Source:      "direct",
		})
		require.Equal(t, []string{"github:alice", "writer", "direct", "account"}, row)
	})

	// Org membership is the same table shape at the front: the grantee's
	// handle first, the account ULID only when the server sent no handle.
	t.Run("org member shows the handle", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, []string{"GRANTEE", "NAME", "ROLE", "STATUS"}, orgMemberColumns)
		row := orgMemberRow(coreapi.OrgMemberListItem{AccountId: ulid, Handle: coreapi.NewOptString("github:alice"), Role: "owner", Status: "active"})
		require.Equal(t, []string{"github:alice", "-", "owner", "active"}, row)
	})

	t.Run("org member without a handle falls back to the ULID", func(t *testing.T) {
		t.Parallel()
		row := orgMemberRow(coreapi.OrgMemberListItem{AccountId: ulid, Role: "member", Status: "pending"})
		require.Equal(t, []string{ulid, "-", "member", "pending"}, row)
	})

	// A Google handle is only a subject id, so the display name the server
	// sends is what names the person.
	t.Run("org member shows the display name", func(t *testing.T) {
		t.Parallel()
		row := orgMemberRow(coreapi.OrgMemberListItem{
			AccountId: ulid, Handle: coreapi.NewOptString("google:google-1001"),
			DisplayName: coreapi.NewOptString("Victor Gutierrez"), Role: "writer", Status: "active",
		})
		require.Equal(t, []string{"google:1001", "Victor Gutierrez", "writer", "active"}, row)
	})

	t.Run("repo unresolved name falls back to ULID", func(t *testing.T) {
		t.Parallel()
		row := repoGrantRow(coreapi.RepoGrant{
			GranteeId:   ulid,
			GranteeName: coreapi.OptString{},
			GranteeType: "team",
			Role:        "reader",
			Source:      "inherited",
		})
		require.Equal(t, []string{ulid, "reader", "inherited", "team"}, row)
	})
}

// A grantee held several ways is one table row: its strongest role, and every
// way it holds the target with the role each carries, so revoking the direct
// grant visibly leaves the inherited one. Rows held once are untouched.
func TestMergeRepoGrants(t *testing.T) {
	t.Parallel()
	rows := []coreapi.RepoGrant{
		{GranteeType: granteeTypeAccount, GranteeId: "acct-a", GranteeName: coreapi.NewOptString("github:alice"), Role: "admin", Source: "direct"},
		{GranteeType: "org", GranteeId: "org-1", GranteeName: coreapi.NewOptString("acme"), Role: "owner", Source: "owner"},
		{GranteeType: granteeTypeAccount, GranteeId: "acct-a", GranteeName: coreapi.NewOptString("github:alice"), Role: "reader", Source: "project:web"},
		{GranteeType: granteeTypeAccount, GranteeId: "acct-b", GranteeName: coreapi.NewOptString("github:bob"), Role: "writer", Source: "project:web"},
	}
	got := mergeRepoGrants(rows)
	require.Len(t, got, 3)
	require.Equal(t, []string{"github:alice", "admin", "direct (admin), project:web (reader)", "account"}, repoGrantRow(got[0]))
	require.Equal(t, []string{"acme", "owner", "owner", "org"}, repoGrantRow(got[1]))
	require.Equal(t, []string{"github:bob", "writer", "project:web", "account"}, repoGrantRow(got[2]))
	// The strongest role wins whichever row came first.
	weakFirst := mergeRepoGrants([]coreapi.RepoGrant{rows[2], rows[0]})
	require.Equal(t, "admin", weakFirst[0].Role)
	require.Equal(t, "project:web (reader), direct (admin)", weakFirst[0].Source)
}

// The table folds a grantee held two ways into one row; --json keeps the wire
// rows as sent, since merging is the table's reading, not the data's.
//
// Not parallel: swaps the activeCoreClient seam.
func TestRepoGrantList_MergesTheTableNotTheJSON(t *testing.T) {
	both := holder{id: "acct-a", handle: "github:alice"}
	srv := pickerServer(t, pickerFixture{held: []holder{both}, viaProject: []holder{both}}, &[]string{}, nil)
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", wiringRepoPath)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(out, "github:alice"), "table: one row per grantee\n%s", out)
	require.Contains(t, out, "direct (writer), project:widgets (writer)")

	out, _, err = runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", wiringRepoPath, "--json")
	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 2, "--json keeps both wire rows")
}
