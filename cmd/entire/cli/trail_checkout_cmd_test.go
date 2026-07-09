package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestResolveTrailBySelector_FindsBySelector(t *testing.T) {
	// Not t.Parallel(): the subtests share one httptest server closed on
	// return, so they must run synchronously before the deferred Close.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{
			Trails: []api.TrailResource{
				{ID: "trl_a", Number: 1, Branch: "feature/a", Title: "Alpha"},
				{ID: "trl_b", Number: 575, Branch: "feature/b", Title: "Bravo"},
			},
			Total: 2,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)

	cases := []struct {
		name     string
		selector string
		wantID   string
	}{
		{"by number", "575", "trl_b"},
		{"by id", "trl_a", "trl_a"},
		{"by branch", "feature/b", "trl_b"},
		{"trims whitespace", "  feature/a  ", "trl_a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := resolveTrailBySelector(context.Background(), client, "gh", "acme", "repo", tc.selector, "")
			if err != nil {
				t.Fatalf("resolveTrailBySelector: %v", err)
			}
			if found == nil || found.ID != tc.wantID {
				t.Fatalf("found = %#v, want ID %q", found, tc.wantID)
			}
		})
	}
}

func TestResolveTrailBySelector_NotFoundIsAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{}}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := resolveTrailBySelector(context.Background(), client, "gh", "acme", "repo", "does-not-exist", "")
	if err == nil {
		t.Fatalf("expected error for missing trail, got found = %#v", found)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil on error", found)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error %q should name the selector", err)
	}
}

func TestDescribeTrailRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   api.TrailResource
		want string
	}{
		{"number and title", api.TrailResource{Number: 575, Title: "Add foo"}, "trail #575 (Add foo)"},
		{"number without title", api.TrailResource{Number: 575}, "trail #575"},
		{"title without number", api.TrailResource{Title: "Add foo"}, `trail "Add foo"`},
		{"neither", api.TrailResource{}, "trail"},
		{"title trimmed", api.TrailResource{Number: 1, Title: "  Add foo  "}, "trail #1 (Add foo)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Copy the input into a local so the parallel subtest never takes the
			// address of the shared range variable.
			in := tc.in
			got := describeTrailRef(&in)
			if got != tc.want {
				t.Fatalf("describeTrailRef(%#v) = %q, want %q", in, got, tc.want)
			}
		})
	}
}

func TestDefaultTrailWorktreePath(t *testing.T) {
	t.Parallel()

	got := defaultTrailWorktreePath("/repo", "peter/feature.auth", 123)
	want := filepath.Join("/repo", ".entire", "worktrees", "trail-123-peter-feature.auth")
	if got != want {
		t.Fatalf("defaultTrailWorktreePath() = %q, want %q", got, want)
	}
}

func TestShellQuotePath(t *testing.T) {
	t.Parallel()

	got := shellQuotePath("/tmp/it's here")
	want := `'/tmp/it'\''s here'`
	if got != want {
		t.Fatalf("shellQuotePath() = %q, want %q", got, want)
	}
}

func TestCheckoutTrailWorktreeCreatesRepoLocalWorktree(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "test\n")
	runTrailCheckoutGit(t, repoDir, "add", "README.md")
	runTrailCheckoutGit(t, repoDir, "commit", "-m", "initial")
	runTrailCheckoutGit(t, repoDir, "branch", "feature/test")
	startBranch := currentBranchTrailCheckoutTest(t, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/test", true, 7)
	if err != nil {
		t.Fatalf("checkoutTrailWorktree: %v; stderr: %s", err, errOut.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-7-feature-test")
	if !strings.Contains(out.String(), wantPath) {
		t.Fatalf("output = %q, want path %q", out.String(), wantPath)
	}
	if !strings.Contains(out.String(), "cd '") || !strings.Contains(out.String(), "trail-7-feature-test'") {
		t.Fatalf("output = %q, want cd hint", out.String())
	}
	if got := currentBranchTrailCheckoutTest(t, repoDir); got != startBranch {
		t.Fatalf("current branch = %q, want %s", got, startBranch)
	}
	if got := currentBranchTrailCheckoutTest(t, wantPath); got != "feature/test" {
		t.Fatalf("worktree branch = %q, want feature/test", got)
	}
	excludeBytes, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	exclude := string(excludeBytes)
	if !strings.Contains(exclude, ".entire/worktrees/") {
		t.Fatalf("exclude = %q, want .entire/worktrees/", exclude)
	}
}

func TestCheckoutTrailWorktreeFromLinkedWorktreeCreatesSiblingUnderMainRoot(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "test\n")
	runTrailCheckoutGit(t, repoDir, "add", "README.md")
	runTrailCheckoutGit(t, repoDir, "commit", "-m", "initial")
	runTrailCheckoutGit(t, repoDir, "branch", "feature/first")
	runTrailCheckoutGit(t, repoDir, "branch", "feature/second")
	t.Chdir(repoDir)

	var firstOut, firstErr bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &firstOut, &firstErr, "feature/first", true, 7); err != nil {
		t.Fatalf("checkoutTrailWorktree first: %v; stderr: %s", err, firstErr.String())
	}
	firstPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-7-feature-first")
	t.Chdir(firstPath)

	var secondOut, secondErr bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &secondOut, &secondErr, "feature/second", true, 8); err != nil {
		t.Fatalf("checkoutTrailWorktree second: %v; stderr: %s", err, secondErr.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-8-feature-second")
	nestedPath := filepath.Join(firstPath, ".entire", "worktrees", "trail-8-feature-second")
	if !strings.Contains(secondOut.String(), wantPath) {
		t.Fatalf("output = %q, want main-root path %q", secondOut.String(), wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("sibling worktree missing: %v", err)
	}
	if _, err := os.Stat(nestedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested worktree stat error = %v, want not exist", err)
	}
}

func TestEnsureTrailWorktreeLocalExcludeIsIdempotent(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	if err := ensureTrailWorktreeLocalExclude(context.Background()); err != nil {
		t.Fatalf("ensureTrailWorktreeLocalExclude first: %v", err)
	}
	if err := ensureTrailWorktreeLocalExclude(context.Background()); err != nil {
		t.Fatalf("ensureTrailWorktreeLocalExclude second: %v", err)
	}

	excludeBytes, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if got := strings.Count(string(excludeBytes), ".entire/worktrees/"); got != 1 {
		t.Fatalf("exclude rule count = %d, want 1; content: %q", got, string(excludeBytes))
	}
}

func TestCheckoutTrailWorktreeFetchesRemoteOnlyBranch(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	seedDir := filepath.Join(tmp, "seed")
	repoDir := filepath.Join(tmp, "local")
	runTrailCheckoutGit(t, tmp, "init", "--bare", originDir)
	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "README.md", "test\n")
	runTrailCheckoutGit(t, seedDir, "add", "README.md")
	runTrailCheckoutGit(t, seedDir, "commit", "-m", "initial")
	runTrailCheckoutGit(t, seedDir, "checkout", "-b", "feature/remote")
	testutil.WriteFile(t, seedDir, "remote.txt", "remote\n")
	runTrailCheckoutGit(t, seedDir, "add", "remote.txt")
	runTrailCheckoutGit(t, seedDir, "commit", "-m", "remote branch")
	runTrailCheckoutGit(t, seedDir, "remote", "add", "origin", originDir)
	runTrailCheckoutGit(t, seedDir, "push", "origin", "master", "feature/remote")
	runTrailCheckoutGit(t, tmp, "clone", originDir, repoDir)
	t.Chdir(repoDir)
	startBranch := currentBranchTrailCheckoutTest(t, repoDir)

	var out, errOut bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/remote", true, 12); err != nil {
		t.Fatalf("checkoutTrailWorktree: %v; stderr: %s", err, errOut.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-12-feature-remote")
	if got := currentBranchTrailCheckoutTest(t, repoDir); got != startBranch {
		t.Fatalf("current branch = %q, want %s", got, startBranch)
	}
	if got := currentBranchTrailCheckoutTest(t, wantPath); got != "feature/remote" {
		t.Fatalf("worktree branch = %q, want feature/remote", got)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "remote.txt")); err != nil {
		t.Fatalf("remote branch file missing: %v", err)
	}
}

func TestCheckoutTrailWorktreeReusesExistingWorktree(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "test\n")
	runTrailCheckoutGit(t, repoDir, "add", "README.md")
	runTrailCheckoutGit(t, repoDir, "commit", "-m", "initial")
	runTrailCheckoutGit(t, repoDir, "branch", "feature/reuse")
	t.Chdir(repoDir)

	var firstOut, firstErr bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &firstOut, &firstErr, "feature/reuse", true, 9); err != nil {
		t.Fatalf("checkoutTrailWorktree first: %v; stderr: %s", err, firstErr.String())
	}
	var secondOut, secondErr bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &secondOut, &secondErr, "feature/reuse", true, 9); err != nil {
		t.Fatalf("checkoutTrailWorktree second: %v; stderr: %s", err, secondErr.String())
	}
	if !strings.Contains(secondOut.String(), "Worktree already exists") {
		t.Fatalf("second output = %q, want existing worktree message", secondOut.String())
	}
}

func TestCheckoutTrailWorktreeRejectsInvalidBranch(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "-bad", true, 1)
	if err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("error = %v, want invalid branch", err)
	}
}

func TestTrailCheckoutRejectsArgWithTrailFlag(t *testing.T) {
	t.Parallel()

	cmd := newTrailCheckoutCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"feature/b", "--trail", "575"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error combining a positional arg with --trail, got nil")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Fatalf("error = %q, want it to mention 'cannot combine'", err)
	}
}

func runTrailCheckoutGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
}

func currentBranchTrailCheckoutTest(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "branch", "--show-current")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --show-current failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}
