package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestResolveDefaultSearchRepo pins that semantic search derives its default
// (current-repo) scope the way the --code path does: from the origin remote's
// forge-qualified coordinates, so an Entire-native clone is a valid place to
// search from and is never rejected as "not a GitHub repository".
//
// Not parallel: t.Chdir points ResolveRemoteRepo at the fixture repo.
func TestResolveDefaultSearchRepo(t *testing.T) {
	tests := []struct {
		name      string
		origin    string // empty: no origin remote
		explicit  bool
		wantForge string
		wantOwner string
		wantRepo  string
		wantErr   string
	}{
		{name: "native origin", origin: "entire://cell1.entire.io/et/audit1/foo.git", wantForge: "et", wantOwner: "audit1", wantRepo: "foo"},
		{name: "github origin", origin: "https://github.com/acme/thing.git", wantForge: "gh", wantOwner: "acme", wantRepo: "thing"},
		{name: "mirror origin", origin: "entire://cell1.entire.io/gh/acme/thing", wantForge: "gh", wantOwner: "acme", wantRepo: "thing"},
		{name: "native origin with explicit scope still resolves", origin: "entire://cell1.entire.io/et/audit1/foo", explicit: true, wantForge: "et", wantOwner: "audit1", wantRepo: "foo"},
		{name: "no origin without explicit scope is an error", wantErr: "use --repo or --all-repos"},
		{name: "no origin with explicit scope is not an error", explicit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			runExpertsGit(t, dir, "init")
			if tt.origin != "" {
				runExpertsGit(t, dir, "remote", "add", "origin", tt.origin)
			}
			t.Chdir(dir)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)

			forge, owner, repo, err := resolveDefaultSearchRepo(context.Background(), tt.explicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if forge != tt.wantForge || owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", forge, owner, repo, tt.wantForge, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}
