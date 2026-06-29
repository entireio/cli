package cli

import (
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

func TestParseGranteeMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                                 string
		provider, providerUserID, gType, gID string
		want                                 granteeMode
		wantErr                              bool
	}{
		{name: "provider mode", provider: "github", providerUserID: "123", want: granteeModeProvider},
		{name: "id mode", gType: "org", gID: "01J0", want: granteeModeID},
		{name: "both modes rejected", provider: "github", providerUserID: "123", gType: "org", gID: "01J0", wantErr: true},
		{name: "partial provider", provider: "github", wantErr: true},
		{name: "partial id", gType: "org", wantErr: true},
		{name: "nothing", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseGranteeMode(tt.provider, tt.providerUserID, tt.gType, tt.gID)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGranteeMode(%q,%q,%q,%q) expected error", tt.provider, tt.providerUserID, tt.gType, tt.gID)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGranteeMode: %v", err)
			}
			if got != tt.want {
				t.Errorf("got mode %d, want %d", got, tt.want)
			}
		})
	}
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
