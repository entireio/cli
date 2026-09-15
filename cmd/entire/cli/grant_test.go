package cli

import (
	"slices"
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

// TestGrantTargetRoles pins each target's role set and default against the
// server's enums: org membership has owner/admin/member with member as the
// server default, while project and repo access has reader/writer/admin and no
// default, so --role is required there.
func TestGrantTargetRoles(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"owner", "admin", "member"}, orgGrantTarget.roles)
	require.Equal(t, "member", orgGrantTarget.defaultRole)
	require.Equal(t, []string{"reader", "writer", "admin"}, projectGrantTarget.roles)
	require.Empty(t, projectGrantTarget.defaultRole)
	require.Equal(t, []string{"reader", "writer", "admin"}, repoGrantTarget.roles)
	require.Empty(t, repoGrantTarget.defaultRole)
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
	// same column order — or the table header and cells misalign.
	if got, want := len(grantColumns), 5; got != want {
		t.Fatalf("grantColumns has %d columns, want %d", got, want)
	}

	t.Run("project resolved name", func(t *testing.T) {
		t.Parallel()
		row := projectGrantRow(coreapi.ProjectGrant{
			GranteeId:   ulid,
			GranteeName: coreapi.NewOptString("github:alice"),
			GranteeType: "account",
			Role:        "writer",
			Source:      "direct",
		})
		want := []string{"github:alice", "writer", "direct", "account", ulid}
		if !slices.Equal(row, want) {
			t.Errorf("projectGrantRow = %v, want %v", row, want)
		}
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
		want := []string{ulid, "reader", "inherited", "team", ulid}
		if !slices.Equal(row, want) {
			t.Errorf("repoGrantRow = %v, want %v", row, want)
		}
	})
}
