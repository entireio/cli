package cli

import (
	"slices"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestValidateGrantRole(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"reader", "writer", "admin"} {
		if err := validateGrantRole(ok); err != nil {
			t.Errorf("validateGrantRole(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "owner", "Reader", "member"} {
		if err := validateGrantRole(bad); err == nil {
			t.Errorf("validateGrantRole(%q) expected error", bad)
		}
	}
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

func TestParseOrgRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    coreapi.AddOrgMemberInputBodyRole
		wantErr bool
	}{
		{in: "owner", want: coreapi.AddOrgMemberInputBodyRoleOwner},
		{in: "admin", want: coreapi.AddOrgMemberInputBodyRoleAdmin},
		{in: "member", want: coreapi.AddOrgMemberInputBodyRoleMember},
		{in: "", wantErr: true},
		{in: "viewer", wantErr: true},
		{in: "Owner", wantErr: true}, // case-sensitive: server enum is lowercase
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseOrgRole(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseOrgRole(%q) expected error, got %q", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOrgRole(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseOrgRole(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
