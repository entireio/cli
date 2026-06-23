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

func TestPickOrg(t *testing.T) {
	t.Parallel()
	orgs := []coreapi.Org{
		{ID: "01J0ORG0000000000000000001", Name: "acme"},
		{ID: "01J0ORG0000000000000000002", Name: "globex"},
	}

	t.Run("unique match", func(t *testing.T) {
		t.Parallel()
		got, err := pickOrg(orgs, "globex")
		if err != nil {
			t.Fatalf("pickOrg: %v", err)
		}
		if got != "01J0ORG0000000000000000002" {
			t.Errorf("pickOrg = %q, want globex id", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		if _, err := pickOrg(orgs, "missing"); err == nil {
			t.Error("pickOrg expected error for unknown name")
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		t.Parallel()
		dupes := []coreapi.Org{
			{ID: "01J0ORG000000000000000000A", Name: "dup"},
			{ID: "01J0ORG000000000000000000B", Name: "dup"},
		}
		if _, err := pickOrg(dupes, "dup"); err == nil {
			t.Error("pickOrg expected error for ambiguous name")
		}
	})
}

func TestPickProject(t *testing.T) {
	t.Parallel()
	projects := []coreapi.Project{
		{ID: "01J0PRJ0000000000000000001", Name: "widgets", OwnerId: "01J0ORG0000000000000000001"},
		{ID: "01J0PRJ0000000000000000002", Name: "gadgets", OwnerId: "01J0ORG0000000000000000001"},
	}

	t.Run("unique match", func(t *testing.T) {
		t.Parallel()
		got, err := pickProject(projects, "gadgets")
		if err != nil {
			t.Fatalf("pickProject: %v", err)
		}
		if got != "01J0PRJ0000000000000000002" {
			t.Errorf("pickProject = %q, want gadgets id", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		if _, err := pickProject(projects, "missing"); err == nil {
			t.Error("pickProject expected error for unknown name")
		}
	})

	t.Run("ambiguous across owners", func(t *testing.T) {
		t.Parallel()
		dupes := []coreapi.Project{
			{ID: "01J0PRJ000000000000000000A", Name: "shared", OwnerId: "01J0ORG0000000000000000001"},
			{ID: "01J0PRJ000000000000000000B", Name: "shared", OwnerId: "01J0ORG0000000000000000002"},
		}
		if _, err := pickProject(dupes, "shared"); err == nil {
			t.Error("pickProject expected error for ambiguous name")
		}
	})
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
