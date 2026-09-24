package cli

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// TestValidateRole covers the one role check every `<noun> grant add` runs:
// the value matches one of the target's roles exactly (the server enums are
// lowercase) and the message lists what would have been accepted.
func TestValidateRole(t *testing.T) {
	t.Parallel()
	roles := []string{"reader", "writer", "admin"}
	for _, ok := range roles {
		require.NoError(t, validateRole(ok, roles))
	}
	for _, bad := range []string{"", "owner", "Reader", "member"} {
		require.ErrorContains(t, validateRole(bad, roles), "invalid --role "+strconv.Quote(bad)+": must be one of reader, writer, admin")
	}
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
		require.Equal(t, []string{"GRANTEE", "ROLE", "STATUS"}, orgMemberColumns)
		row := orgMemberRow(coreapi.Membership{AccountId: ulid, Handle: coreapi.NewOptString("github:alice"), Role: "owner", Status: "active"})
		require.Equal(t, []string{"github:alice", "owner", "active"}, row)
	})

	t.Run("org member without a handle falls back to the ULID", func(t *testing.T) {
		t.Parallel()
		row := orgMemberRow(coreapi.Membership{AccountId: ulid, Role: "member", Status: "pending"})
		require.Equal(t, []string{ulid, "member", "pending"}, row)
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
