package cli

import (
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestParseProjectOwnerType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    coreapi.CreateProjectInputBodyOwnerType
		wantErr bool
	}{
		{in: "org", want: coreapi.CreateProjectInputBodyOwnerTypeOrg},
		{in: "account", want: coreapi.CreateProjectInputBodyOwnerTypeAccount},
		{in: "", wantErr: true},
		{in: "team", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseProjectOwnerType(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseProjectOwnerType(%q) expected error, got %q", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProjectOwnerType(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseProjectOwnerType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
