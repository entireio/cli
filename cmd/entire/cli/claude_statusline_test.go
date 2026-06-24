package cli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestRenderStatusline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		result     statuslineResult
		wantSubstr []string
		wantEmpty  bool
	}{
		{
			name:       "found with status and url",
			result:     statuslineResult{Status: "found", Number: 7, TrailStatus: "open", URL: "https://entire.io/gh/acme/widgets/trails/7"},
			wantSubstr: []string{"Trail #7", "open", "https://entire.io/gh/acme/widgets/trails/7", ansiCyan},
		},
		{
			name:       "found without status",
			result:     statuslineResult{Status: "found", Number: 3},
			wantSubstr: []string{"Trail #3"},
		},
		{
			name:       "auth",
			result:     statuslineResult{Status: "auth"},
			wantSubstr: []string{"entire login"},
		},
		{
			name:      "no-trail renders empty",
			result:    statuslineResult{Status: "no-trail"},
			wantEmpty: true,
		},
		{
			name:       "error with message",
			result:     statuslineResult{Status: "error", Message: "boom"},
			wantSubstr: []string{"Trail:", "boom"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := renderStatusline(tt.result)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("renderStatusline() = %q, want empty", got)
				}
				return
			}
			for _, sub := range tt.wantSubstr {
				if !strings.Contains(got, sub) {
					t.Errorf("renderStatusline() = %q, want substring %q", got, sub)
				}
			}
		})
	}
}

func TestStatuslineTrailURL(t *testing.T) {
	t.Parallel()
	got := statuslineTrailURL("gh", "acme", "widgets", &api.TrailResource{Number: 12, Title: "Add Login Flow"}, "feature")
	want := "https://entire.io/gh/acme/widgets/trails/12/add-login-flow"
	if got != want {
		t.Fatalf("statuslineTrailURL() = %q, want %q", got, want)
	}

	// Falls back to branch when title is empty.
	got = statuslineTrailURL("gh", "acme", "widgets", &api.TrailResource{Number: 4}, "my-branch")
	if want := "https://entire.io/gh/acme/widgets/trails/4/my-branch"; got != want {
		t.Fatalf("statuslineTrailURL() = %q, want %q", got, want)
	}

	// Number 0 (not a real trail) → empty.
	if got := statuslineTrailURL("gh", "acme", "widgets", &api.TrailResource{Number: 0}, "b"); got != "" {
		t.Fatalf("statuslineTrailURL() = %q, want empty for number 0", got)
	}
}

func TestStatuslineCacheFile(t *testing.T) {
	t.Parallel()
	a := statuslineCacheFile("gh", "acme", "widgets", "main")
	b := statuslineCacheFile("gh", "acme", "widgets", "main")
	if a != b {
		t.Fatalf("cache file not stable: %q vs %q", a, b)
	}
	if c := statuslineCacheFile("gh", "acme", "widgets", "other"); c == a {
		t.Fatal("expected different cache file for different branch")
	}
	if !strings.HasSuffix(a, ".json") {
		t.Errorf("cache file %q does not end in .json", a)
	}
}

func TestReadStatuslineCWD(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, input, want string
	}{
		{"workspace preferred", `{"cwd":"/a","workspace":{"current_dir":"/b"}}`, "/b"},
		{"cwd fallback", `{"cwd":"/a"}`, "/a"},
		{"empty", ``, ""},
		{"garbage", `not json`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := readStatuslineCWD(strings.NewReader(tt.input)); got != tt.want {
				t.Fatalf("readStatuslineCWD(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStatuslineCacheRoundTrip(t *testing.T) {
	t.Parallel()
	cacheFile := filepath.Join(t.TempDir(), "x.json")

	// Missing cache → nil, infinite age.
	if r, age := readStatuslineCache(cacheFile); r != nil || age != statuslineInfiniteAge {
		t.Fatalf("missing cache: got (%v, %v)", r, age)
	}

	writeStatuslineCache(cacheFile, statuslineResult{Status: "found", Number: 9, TrailStatus: "open"})
	r, age := readStatuslineCache(cacheFile)
	if r == nil {
		t.Fatal("expected cached result after write")
	}
	if r.Number != 9 || r.Status != "found" {
		t.Fatalf("round-trip mismatch: %+v", r)
	}
	if age > statuslineFreshDuration {
		t.Fatalf("freshly written cache reported stale age %v", age)
	}
}

func TestIsStatuslineAuthError(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{"not logged in (run 'entire login')", "HTTP 401 Unauthorized", "authentication required"} {
		if !isStatuslineAuthError(msg) {
			t.Errorf("isStatuslineAuthError(%q) = false, want true", msg)
		}
	}
	if isStatuslineAuthError("connection refused") {
		t.Error("isStatuslineAuthError(connection refused) = true, want false")
	}
}

func TestShortStatuslineError(t *testing.T) {
	t.Parallel()
	if got := shortStatuslineError("first line\nsecond line"); got != "first line" {
		t.Errorf("shortStatuslineError multiline = %q", got)
	}
	if got := shortStatuslineError(""); got != "lookup failed" {
		t.Errorf("shortStatuslineError empty = %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := shortStatuslineError(long); len(got) != 60 {
		t.Errorf("shortStatuslineError long len = %d, want 60", len(got))
	}
}

// setupStatuslineRepo creates an isolated git repo with a github origin and one
// commit, chdir's into it, and returns the resolved forge/owner/repo/branch.
// Not parallel-safe (uses t.Chdir).
func setupStatuslineRepo(t *testing.T, repoName string) (forge, owner, repo, branch, repoDir string) {
	t.Helper()
	repoDir = t.TempDir()
	testutil.InitRepo(t, repoDir)

	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@github.com:acme/"+repoName+".git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}

	testutil.WriteFile(t, repoDir, "f.txt", "hi")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)

	ctx := context.Background()
	var err error
	branch, err = GetCurrentBranch(ctx)
	if err != nil {
		t.Fatalf("GetCurrentBranch: %v", err)
	}
	forge, owner, repo, err = gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		t.Fatalf("ResolveRemoteRepo: %v", err)
	}
	return forge, owner, repo, branch, repoDir
}

func TestClaudeStatuslineServe_FreshCacheHit(t *testing.T) {
	forge, owner, repo, branch, repoDir := setupStatuslineRepo(t, "widgets")

	// Refresh must NOT be spawned for a fresh cache.
	orig := spawnStatuslineRefresh
	t.Cleanup(func() { spawnStatuslineRefresh = orig })
	spawned := false
	spawnStatuslineRefresh = func(_, _ string) { spawned = true }

	writeStatuslineCache(statuslineCacheFile(forge, owner, repo, branch), statuslineResult{
		Status: "found", Number: 7, TrailStatus: "open", URL: "https://entire.io/gh/acme/widgets/trails/7",
	})

	out := runStatuslineServeCmd(t, repoDir)
	if !strings.Contains(out, "Trail #7") || !strings.Contains(out, "trails/7") {
		t.Fatalf("serve output = %q, want Trail #7 link", out)
	}
	if spawned {
		t.Error("refresh was spawned for a fresh cache")
	}
}

func TestClaudeStatuslineServe_StaleCacheSpawnsRefresh(t *testing.T) {
	_, _, _, _, repoDir := setupStatuslineRepo(t, "gadgets")

	orig := spawnStatuslineRefresh
	t.Cleanup(func() { spawnStatuslineRefresh = orig })
	var gotCacheFile string
	spawnStatuslineRefresh = func(_, cacheFile string) { gotCacheFile = cacheFile }

	// No cache seeded → stale → refresh spawned, output empty.
	out := runStatuslineServeCmd(t, repoDir)
	if out != "" {
		t.Fatalf("serve output = %q, want empty on cache miss", out)
	}
	if gotCacheFile == "" {
		t.Fatal("expected refresh to be spawned on cache miss")
	}
}

// runStatuslineServeCmd executes the statusline serve path with stdin pointing
// at repoDir and returns captured stdout.
func runStatuslineServeCmd(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := newClaudeStatuslineCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader(`{"workspace":{"current_dir":"` + repoDir + `"}}`))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("statusline serve execute: %v", err)
	}
	return buf.String()
}
