package cli

import (
	"encoding/json"
	"testing"

	"github.com/go-faster/jx"

	"github.com/entireio/cli/internal/coreapi"
)

func TestRepoRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		repo coreapi.Repo
		want string
	}{
		{
			name: "host and path produce an entire:// URL",
			repo: coreapi.Repo{
				ClusterHost: coreapi.NewOptString("aws-us-east-2.entire.io"),
				Path:        coreapi.NewOptString("acme/web"),
			},
			want: "entire://aws-us-east-2.entire.io/acme/web",
		},
		{
			name: "leading slash on path is not doubled",
			repo: coreapi.Repo{
				ClusterHost: coreapi.NewOptString("aws-us-east-2.entire.io"),
				Path:        coreapi.NewOptString("/acme/web"),
			},
			want: "entire://aws-us-east-2.entire.io/acme/web",
		},
		{
			name: "missing host yields no URL",
			repo: coreapi.Repo{Path: coreapi.NewOptString("acme/web")},
			want: "",
		},
		{
			name: "missing path yields no URL",
			repo: coreapi.Repo{ClusterHost: coreapi.NewOptString("aws-us-east-2.entire.io")},
			want: "",
		},
		{
			name: "blank coordinates yield no URL",
			repo: coreapi.Repo{
				ClusterHost: coreapi.NewOptString("  "),
				Path:        coreapi.NewOptString(""),
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := repoRemoteURL(tt.repo); got != tt.want {
				t.Errorf("repoRemoteURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseVisibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    coreapi.SetRepoVisibilityInputBodyVisibility
		wantErr bool
	}{
		{in: "public", want: coreapi.SetRepoVisibilityInputBodyVisibilityPublic},
		{in: "private", want: coreapi.SetRepoVisibilityInputBodyVisibilityPrivate},
		{in: "Public", wantErr: true}, // case-sensitive; the wire enum is lowercase
		{in: "", wantErr: true},
		{in: "world-readable", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseVisibility(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseVisibility(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVisibility(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseVisibility(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRepoDetailRow(t *testing.T) {
	t.Parallel()

	t.Run("includes the entire:// remote", func(t *testing.T) {
		t.Parallel()
		row := repoDetailRow(coreapi.Repo{
			ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
			Name:            "web",
			OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
			ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
			Path:            coreapi.NewOptString("acme/web"),
			State:           coreapi.NewOptRepoState(coreapi.RepoStateActive),
		})
		if len(row) != len(repoDetailColumns) {
			t.Fatalf("row has %d cells, want %d (one per column)", len(row), len(repoDetailColumns))
		}
		if want := "entire://aws-us-east-2.entire.io/acme/web"; row[len(row)-1] != want {
			t.Errorf("REMOTE cell = %q, want %q", row[len(row)-1], want)
		}
	})

	t.Run("shows - when the remote is not yet resolvable", func(t *testing.T) {
		t.Parallel()
		row := repoDetailRow(coreapi.Repo{
			ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
			Name:            "web",
			OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
			ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
		})
		if row[len(row)-1] != "-" {
			t.Errorf("REMOTE cell = %q, want %q", row[len(row)-1], "-")
		}
	})
}

func TestRepoCreateOutput_StampsRemote(t *testing.T) {
	t.Parallel()
	repo := &coreapi.Repo{
		ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
		Name:            "web",
		OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
		ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
		Path:            coreapi.NewOptString("acme/web"),
	}
	out, err := repoCreateOutput(repo)
	if err != nil {
		t.Fatalf("repoCreateOutput() error = %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if want := "entire://aws-us-east-2.entire.io/acme/web"; got["remote"] != want {
		t.Errorf("remote = %v, want %q", got["remote"], want)
	}
	// The original repo fields must survive the round-trip alongside the
	// synthesized remote.
	if got["id"] != repo.ID {
		t.Errorf("id = %v, want %q", got["id"], repo.ID)
	}
	if got["name"] != repo.Name {
		t.Errorf("name = %v, want %q", got["name"], repo.Name)
	}
}

func TestRepoCreateOutput_PreservesServerProvidedRemote(t *testing.T) {
	t.Parallel()
	// A server-provided `remote` (here via additional properties, the same
	// path a future first-class field would surface through) must win over
	// the synthesized one — synthesis only fills a gap.
	const serverRemote = "entire://override.entire.io/server/value"
	repo := &coreapi.Repo{
		ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
		Name:            "web",
		OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
		ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
		Path:            coreapi.NewOptString("acme/web"),
		AdditionalProps: coreapi.RepoAdditional{
			"remote": jx.Raw(`"` + serverRemote + `"`),
		},
	}
	out, err := repoCreateOutput(repo)
	if err != nil {
		t.Fatalf("repoCreateOutput() error = %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got["remote"] != serverRemote {
		t.Errorf("remote = %v, want server-provided %q", got["remote"], serverRemote)
	}
}

func TestRepoCreateOutput_NilRepoErrors(t *testing.T) {
	t.Parallel()
	// Defends the contract rather than a real path (the caller only passes a
	// repo after a nil-error create): a nil pointer must return an error, not
	// panic on the later dereference.
	if _, err := repoCreateOutput(nil); err == nil {
		t.Fatal("expected an error for a nil repo, got nil")
	}
}

func TestRepoCreateOutput_OmitsRemoteWhenUnresolvable(t *testing.T) {
	t.Parallel()
	// A still-provisioning repo may lack a path; omit the field rather than
	// emit a half-formed URL.
	repo := &coreapi.Repo{
		ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
		Name:            "web",
		OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
		ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
	}
	out, err := repoCreateOutput(repo)
	if err != nil {
		t.Fatalf("repoCreateOutput() error = %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if _, ok := got["remote"]; ok {
		t.Errorf("expected no remote field, got %v", got["remote"])
	}
}
