package cli

import (
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestLooksLikeULID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{in: "01J0ABCDEFGHJKMNPQRSTVWXYZ", want: true}, // 26 chars, valid alphabet
		{in: "01j0abcdefghjkmnpqrstvwxyz", want: true}, // lowercase accepted
		{in: "acme", want: false},                      // short name
		{in: "my-project", want: false},                // hyphen not in alphabet
		{in: "", want: false},                          // empty
		{in: "01J0ABCDEFGHJKMNPQRSTVWXY", want: false}, // 25 chars
		{in: "01J0ABCDEFGHJKMNPQRSTVWXYZ0", want: false},
		{in: "01J0ABCDEFGHIKMNPQRSTVWXYZ", want: false}, // contains I
		{in: "01J0ABCDEFGHLKMNPQRSTVWXYZ", want: false}, // contains L
		{in: "01J0ABCDEFGHOKMNPQRSTVWXYZ", want: false}, // contains O
		{in: "01J0ABCDEFGHUKMNPQRSTVWXYZ", want: false}, // contains U
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeULID(tt.in); got != tt.want {
				t.Errorf("looksLikeULID(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseQualifiedHandle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in           string
		wantProvider string
		wantHandle   string
		wantErr      bool
	}{
		{in: "github:alice", wantProvider: "github", wantHandle: "alice"},
		{in: "github:alice:bob", wantProvider: "github", wantHandle: "alice:bob"}, // only first colon splits
		{in: "alice", wantErr: true},                                              // no provider prefix
		{in: "github:", wantErr: true},                                            // empty handle
		{in: ":alice", wantErr: true},                                             // empty provider
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			provider, handle, err := parseQualifiedHandle(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseQualifiedHandle(%q) expected error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQualifiedHandle(%q): %v", tt.in, err)
			}
			if provider != tt.wantProvider || handle != tt.wantHandle {
				t.Errorf("parseQualifiedHandle(%q) = (%q, %q), want (%q, %q)", tt.in, provider, handle, tt.wantProvider, tt.wantHandle)
			}
		})
	}
}

func TestFilterProjectsByName(t *testing.T) {
	t.Parallel()
	projects := []coreapi.Project{
		{ID: "1", Name: "a"},
		{ID: "2", Name: "b"},
		{ID: "3", Name: "a"},
	}

	t.Run("empty name returns all", func(t *testing.T) {
		t.Parallel()
		if got := filterProjectsByName(projects, ""); len(got) != 3 {
			t.Errorf("len = %d, want 3", len(got))
		}
	})

	t.Run("exact filter", func(t *testing.T) {
		t.Parallel()
		got := filterProjectsByName(projects, "a")
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		for _, p := range got {
			if p.Name != "a" {
				t.Errorf("unexpected project %q", p.Name)
			}
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		if got := filterProjectsByName(projects, "z"); len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
}
