package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const testEnvFile = ".env"

func TestDefaultTrailWorktreePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		branch      string
		trailNumber int
		want        string
	}{
		{"slash branch", "peter/feature.auth", 123, filepath.Join("/repo", ".entire", "worktrees", "trail-123-peter-feature.auth")},
		{"plain branch", "feature-other", 7, filepath.Join("/repo", ".entire", "worktrees", "trail-7-feature-other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultTrailWorktreePath("/repo", tt.branch, tt.trailNumber); got != tt.want {
				t.Fatalf("defaultTrailWorktreePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeTrailWorktreeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"feature/test", "feature-test"},
		{"Feat_1.2-x", "Feat_1.2-x"},
		{"weird name!", "weird-name"},
		{"---", trailWorktreeFallbackName},
		{"  spaced  ", "spaced"},
	}
	for _, tt := range tests {
		if got := sanitizeTrailWorktreeName(tt.in); got != tt.want {
			t.Fatalf("sanitizeTrailWorktreeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAppendIgnoreRule(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gitignore")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := range 2 {
		appended, err := appendIgnoreRule(path)
		if err != nil {
			t.Fatalf("appendIgnoreRule: %v", err)
		}
		wantAppended := i == 0
		if appended != wantAppended {
			t.Fatalf("appendIgnoreRule appended = %v, want %v", appended, wantAppended)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.Count(string(content), ".entire/worktrees/"); got != 1 {
		t.Fatalf("rule count = %d, want 1; content: %q", got, string(content))
	}
	if !strings.HasSuffix(string(content), "\n") {
		t.Fatalf("content %q missing trailing newline", string(content))
	}
}

func TestAppendIgnoreRule_AddsNewlineBeforeRule(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gitignore")
	if err := os.WriteFile(path, []byte("node_modules"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	appended, err := appendIgnoreRule(path)
	if err != nil {
		t.Fatalf("appendIgnoreRule: %v", err)
	}
	if !appended {
		t.Fatal("appendIgnoreRule appended = false, want true")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(content), "node_modules\n.entire/worktrees/\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestAppendIgnoreRule_MissingFileNoop(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gitignore")
	appended, err := appendIgnoreRule(path)
	if err != nil {
		t.Fatalf("appendIgnoreRule: %v", err)
	}
	if appended {
		t.Fatal("appendIgnoreRule appended = true, want false")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("gitignore stat = %v, want not exist", err)
	}
}

func TestEnsureTrailWorktreeIgnoreRule_AppendsGitignore(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", "node_modules\n")
	t.Chdir(repoDir)

	var out bytes.Buffer
	if err := ensureTrailWorktreeIgnoreRule(context.Background(), &out, repoDir); err != nil {
		t.Fatalf("ensureTrailWorktreeIgnoreRule: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(repoDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(content), ".entire/worktrees/") {
		t.Fatalf(".gitignore = %q, want .entire/worktrees/ rule", string(content))
	}
	if !strings.Contains(out.String(), ".gitignore") {
		t.Fatalf("output = %q, want notice mentioning .gitignore", out.String())
	}
}

func TestEnsureTrailWorktreeIgnoreRule_MissingGitignoreNoop(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	var out bytes.Buffer
	if err := ensureTrailWorktreeIgnoreRule(context.Background(), &out, repoDir); err != nil {
		t.Fatalf("ensureTrailWorktreeIgnoreRule: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want silence", out.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf(".gitignore stat = %v, want not exist", err)
	}
}

func TestEnsureTrailWorktreeIgnoreRule_AlreadyIgnoredIsSilentNoop(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", ".entire/\n")
	t.Chdir(repoDir)

	var out bytes.Buffer
	if err := ensureTrailWorktreeIgnoreRule(context.Background(), &out, repoDir); err != nil {
		t.Fatalf("ensureTrailWorktreeIgnoreRule: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want silence", out.String())
	}
	content, err := os.ReadFile(filepath.Join(repoDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if got, want := string(content), ".entire/\n"; got != want {
		t.Fatalf(".gitignore = %q, want untouched %q", got, want)
	}
}

func TestMatchIncludePatterns(t *testing.T) {
	t.Parallel()

	files := []string{
		testEnvFile,
		"config/.env.local",
		"/abs/.env",
		"../escape/.env",
		"node_modules/pkg/x.js",
	}
	got := matchIncludePatterns([]string{testEnvFile, "*.local"}, files)
	want := []string{testEnvFile, filepath.Join("config", ".env.local")}
	if !slices.Equal(got, want) {
		t.Fatalf("matchIncludePatterns() = %v, want %v", got, want)
	}
}

func TestListIgnoredFiles_ExcludesManagedWorktreePaths(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", ".env\n.entire/\n")
	testutil.WriteFile(t, repoDir, testEnvFile, "SECRET=1\n")
	testutil.WriteFile(t, repoDir, ".entire/worktrees/other/.env", "SECRET=2\n")

	got, err := listIgnoredFiles(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("listIgnoredFiles: %v", err)
	}
	want := []string{testEnvFile}
	if !slices.Equal(got, want) {
		t.Fatalf("listIgnoredFiles() = %v, want %v", got, want)
	}
}

func TestLoadWorktreeIncludePatterns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "# secrets\n\n.env\n*.local\n"
	if err := os.WriteFile(filepath.Join(root, ".worktreeinclude"), []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := loadWorktreeIncludePatterns(root)
	if err != nil {
		t.Fatalf("loadWorktreeIncludePatterns: %v", err)
	}
	want := []string{".env", "*.local"}
	if !slices.Equal(got, want) {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
}

func TestLoadWorktreeIncludePatterns_MissingFile(t *testing.T) {
	t.Parallel()

	got, err := loadWorktreeIncludePatterns(t.TempDir())
	if err != nil {
		t.Fatalf("loadWorktreeIncludePatterns: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("patterns = %v, want none", got)
	}
}

func TestCopyWorktreeIncludeFiles(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, testEnvFile, "SECRET=1\n")
	testutil.WriteFile(t, repoDir, "sub/"+testEnvFile, "SECRET=2\n")
	testutil.GitAdd(t, repoDir, ".gitignore", ".worktreeinclude")
	testutil.GitCommit(t, repoDir, "init")

	dest := t.TempDir()
	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v; stderr: %s", err, errOut.String())
	}
	for _, rel := range []string{testEnvFile, "sub/" + testEnvFile} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("copied file %s missing: %v", rel, err)
		}
	}
}

func TestCopyWorktreeIncludeFiles_NoIncludeFileCopiesNothing(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, testEnvFile, "SECRET=1\n")

	dest := t.TempDir()
	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, testEnvFile)); !os.IsNotExist(err) {
		t.Fatalf("%s stat = %v, want not exist", testEnvFile, err)
	}
}

func TestCopyWorktreeIncludeFiles_SkipsSymlinkWithWarning(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	const symlinkPath = "link.env"
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", symlinkPath+"\ntarget.txt\n")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", symlinkPath+"\n")
	testutil.WriteFile(t, repoDir, "target.txt", "x\n")
	if err := os.Symlink(filepath.Join(repoDir, "target.txt"), filepath.Join(repoDir, symlinkPath)); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dest := t.TempDir()
	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v", err)
	}
	if !strings.Contains(errOut.String(), "warning: skipped "+symlinkPath) {
		t.Fatalf("stderr = %q, want skip warning for %s", errOut.String(), symlinkPath)
	}
	if _, err := os.Stat(filepath.Join(dest, symlinkPath)); !os.IsNotExist(err) {
		t.Fatalf("%s stat = %v, want not exist", symlinkPath, err)
	}
}

func TestCopyWorktreeIncludeFiles_RefusesSymlinkedDirEscape(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", "sub/"+testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", "sub/"+testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, "sub/"+testEnvFile, "SECRET=1\n")

	dest := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "sub")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v", err)
	}
	if !strings.Contains(errOut.String(), "warning: skipped sub/"+testEnvFile) {
		t.Fatalf("stderr = %q, want skip warning for sub/%s", errOut.String(), testEnvFile)
	}
	if _, err := os.Stat(filepath.Join(outside, testEnvFile)); !os.IsNotExist(err) {
		t.Fatalf("%s stat in outside dir = %v, want not exist", testEnvFile, err)
	}
}

func newTrailWorktreeTestRepo(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "test\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "initial")
	return repoDir
}

func currentBranchInDir(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "branch", "--show-current")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --show-current failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func TestCheckoutTrailWorktree_CreatesWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/test")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", ".env\n")
	testutil.WriteFile(t, repoDir, ".env", "SECRET=1\n")
	testutil.WriteFile(t, repoDir, ".gitignore", ".env\n")
	testutil.GitAdd(t, repoDir, ".worktreeinclude", ".gitignore")
	testutil.GitCommit(t, repoDir, "add include config")
	startBranch := currentBranchInDir(t, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/test", false, 7); err != nil {
		t.Fatalf("checkoutTrailWorktree: %v; stderr: %s", err, errOut.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-7-feature-test")
	if got, want := out.String(), wantPath+"\n"; got != want {
		t.Fatalf("stdout = %q, want bare path %q for script use", got, want)
	}
	if !strings.Contains(errOut.String(), "Worktree ready at "+wantPath) {
		t.Fatalf("stderr = %q, want progress notice", errOut.String())
	}
	if got := currentBranchInDir(t, repoDir); got != startBranch {
		t.Fatalf("current branch = %q, want unchanged %q", got, startBranch)
	}
	if got := currentBranchInDir(t, wantPath); got != "feature/test" {
		t.Fatalf("worktree branch = %q, want feature/test", got)
	}
	if _, err := os.Stat(filepath.Join(wantPath, ".env")); err != nil {
		t.Fatalf(".worktreeinclude copy missing: %v", err)
	}
	gitignoreContent, err := os.ReadFile(filepath.Join(repoDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignoreContent), ".entire/worktrees/") {
		t.Fatalf(".gitignore = %q, want .entire/worktrees/ rule", string(gitignoreContent))
	}
}

func TestCheckoutReviewWorktree_CreatesWorktreeWithoutTrail(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/review")
	startBranch := currentBranchInDir(t, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	worktreePath, err := checkoutReviewWorktree(context.Background(), &out, &errOut, "feature/review")
	if err != nil {
		t.Fatalf("checkoutReviewWorktree: %v; stderr: %s", err, errOut.String())
	}
	if worktreePath != defaultReviewWorktreePath(repoDir, "feature/review") {
		t.Fatalf("worktree path = %q, want %q", worktreePath, defaultReviewWorktreePath(repoDir, "feature/review"))
	}
	if got := currentBranchInDir(t, repoDir); got != startBranch {
		t.Fatalf("current branch = %q, want unchanged %q", got, startBranch)
	}
	if got := currentBranchInDir(t, worktreePath); got != "feature/review" {
		t.Fatalf("worktree branch = %q, want feature/review", got)
	}
	if err := removeReviewTarget(context.Background(), worktreePath); err != nil {
		t.Fatalf("removeReviewTarget: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("removed worktree stat = %v, want not exist", err)
	}
}

func TestRemoveReviewTargetKeepsDirtyWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/dirty-review")
	t.Chdir(repoDir)

	worktreePath, err := checkoutReviewWorktree(context.Background(), io.Discard, io.Discard, "feature/dirty-review")
	if err != nil {
		t.Fatalf("checkoutReviewWorktree: %v", err)
	}
	testutil.WriteFile(t, worktreePath, "review-notes.txt", "keep me\n")
	if err := removeReviewTarget(context.Background(), worktreePath); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("removeReviewTarget error = %v, want uncommitted changes", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("dirty worktree was removed: %v", err)
	}
}

func TestCheckoutReviewWorktreeRejectsStaleExternalWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/stale-external")
	externalPath := filepath.Join(t.TempDir(), "external")
	runGit(t, repoDir, "worktree", "add", externalPath, "feature/stale-external")
	if err := os.RemoveAll(externalPath); err != nil {
		t.Fatalf("remove external worktree: %v", err)
	}
	if err := os.MkdirAll(externalPath, 0o750); err != nil {
		t.Fatalf("replace external worktree: %v", err)
	}
	t.Chdir(repoDir)

	_, err := checkoutReviewWorktree(context.Background(), io.Discard, io.Discard, "feature/stale-external")
	if err == nil || !strings.Contains(err.Error(), "git worktree prune") {
		t.Fatalf("checkoutReviewWorktree error = %v, want stale worktree error", err)
	}
}

func TestCheckoutTrailWorktree_FromLinkedWorktreeCreatesSibling(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/first")
	runGit(t, repoDir, "branch", "feature/second")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/first", false, 7); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	firstPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-7-feature-first")
	t.Chdir(firstPath)

	var out2, err2 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/second", false, 8); err != nil {
		t.Fatalf("second checkout: %v; stderr: %s", err, err2.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-8-feature-second")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("sibling worktree missing: %v", err)
	}
	nested := filepath.Join(firstPath, ".entire", "worktrees", "trail-8-feature-second")
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		t.Fatalf("nested worktree stat = %v, want not exist", err)
	}
}

func TestCheckoutTrailWorktree_BranchCheckedOutInMainWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	startBranch := currentBranchInDir(t, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, startBranch, false, 1)
	if err == nil || !strings.Contains(err.Error(), "already checked out at") {
		t.Fatalf("error = %v, want already-checked-out error", err)
	}

	if _, statErr := os.Stat(filepath.Join(repoDir, ".entire", "worktrees")); !os.IsNotExist(statErr) {
		t.Fatalf(".entire/worktrees stat = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, ".gitignore")); !os.IsNotExist(statErr) {
		t.Fatalf(".gitignore stat = %v, want no ignore rule written before the failure", statErr)
	}
}

func TestCheckoutTrailWorktree_ReusesExistingWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/reuse")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/reuse", false, 9); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	var out2, err2 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/reuse", false, 9); err != nil {
		t.Fatalf("second checkout: %v; stderr: %s", err, err2.String())
	}
	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-9-feature-reuse")
	gotPath := strings.TrimSuffix(out2.String(), "\n")
	if strings.Contains(gotPath, "\n") || normalizeWorktreePath(gotPath) != normalizeWorktreePath(wantPath) {
		t.Fatalf("second stdout = %q, want bare path %q for script use", out2.String(), wantPath)
	}
	if !strings.Contains(err2.String(), "Worktree already exists") {
		t.Fatalf("second stderr = %q, want existing-worktree message", err2.String())
	}
}

func TestCheckoutTrailWorktree_FetchesRemoteOnlyBranch(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	seedDir := filepath.Join(tmp, "seed")
	repoDir := filepath.Join(tmp, "local")
	runGit(t, tmp, "init", "--bare", originDir)
	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "README.md", "test\n")
	testutil.GitAdd(t, seedDir, "README.md")
	testutil.GitCommit(t, seedDir, "initial")
	runGit(t, seedDir, "checkout", "-b", "feature/remote")
	testutil.WriteFile(t, seedDir, "remote.txt", "remote\n")
	testutil.GitAdd(t, seedDir, "remote.txt")
	testutil.GitCommit(t, seedDir, "remote branch")
	runGit(t, seedDir, "remote", "add", "origin", originDir)
	runGit(t, seedDir, "push", "origin", "--all")
	runGit(t, tmp, "clone", originDir, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/remote", false, 12); err != nil {
		t.Fatalf("checkoutTrailWorktree: %v; stderr: %s", err, errOut.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-12-feature-remote")
	if got := currentBranchInDir(t, wantPath); got != "feature/remote" {
		t.Fatalf("worktree branch = %q, want feature/remote", got)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "remote.txt")); err != nil {
		t.Fatalf("remote branch file missing: %v", err)
	}
}

func TestCheckoutTrailWorktree_RejectsUnnumberedTrail(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/unnumbered")
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/unnumbered", false, 0)
	if err == nil || !strings.Contains(err.Error(), "has no number yet") {
		t.Fatalf("error = %v, want no-number rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, ".entire", "worktrees")); !os.IsNotExist(statErr) {
		t.Fatalf(".entire/worktrees stat = %v, want not exist", statErr)
	}
}

func TestCheckoutTrailWorktree_StaleManagedWorktreeErrorsWithPruneHint(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/stale")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/stale", false, 4); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	worktreePath := filepath.Join(repoDir, ".entire", "worktrees", "trail-4-feature-stale")
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	var out2, err2 bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/stale", false, 4)
	if err == nil || !strings.Contains(err.Error(), "git worktree prune") {
		t.Fatalf("error = %v, want prune hint", err)
	}
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree path stat = %v, want not recreated", statErr)
	}
}

func TestCheckoutTrailWorktree_StaleManagedWorktreeDirectoryErrorsWithPruneHint(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/stale-dir")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/stale-dir", false, 4); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	worktreePath := filepath.Join(repoDir, ".entire", "worktrees", "trail-4-feature-stale-dir")
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	if err := os.MkdirAll(worktreePath, 0o750); err != nil {
		t.Fatalf("replace worktree dir: %v", err)
	}

	var out2, err2 bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/stale-dir", false, 4)
	if err == nil || !strings.Contains(err.Error(), "git worktree prune") {
		t.Fatalf("error = %v, want prune hint", err)
	}
	if strings.Contains(out2.String(), "Worktree already exists") {
		t.Fatalf("output = %q, want no reuse message", out2.String())
	}
}

func TestCheckoutTrailWorktree_StaleNonManagedWorktreeErrors(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/manual")
	manualPath := filepath.Join(t.TempDir(), "manual")
	runGit(t, repoDir, "worktree", "add", manualPath, "feature/manual")
	if err := os.RemoveAll(manualPath); err != nil {
		t.Fatalf("remove manual worktree: %v", err)
	}
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/manual", false, 5)
	if err == nil || !strings.Contains(err.Error(), "git worktree prune") {
		t.Fatalf("error = %v, want prune hint", err)
	}
}

func TestCheckoutTrailWorktree_RegisteredPathNotADirectory(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/swapped")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/swapped", false, 6); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	worktreePath := filepath.Join(repoDir, ".entire", "worktrees", "trail-6-feature-swapped")
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	if err := os.Symlink(repoDir, worktreePath); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	var out2, err2 bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/swapped", false, 6)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %v, want not-a-directory rejection", err)
	}
}

func TestFindWorktreeForBranch_SurfacesGitError(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	_, _, err := findWorktreeForBranch(context.Background(), "any", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error = %v, want git stderr in message", err)
	}
}

func TestGitCommonDirForTrailWorktree_SurfacesGitError(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	t.Chdir(t.TempDir())

	_, err := gitCommonDirForTrailWorktree(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error = %v, want git stderr in message", err)
	}
}

func TestCheckoutTrailWorktree_RejectsInvalidBranch(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "-bad", false, 1)
	if err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("error = %v, want invalid branch", err)
	}
}

func TestCheckoutTrailWorktree_UnknownBranch(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/nope", false, 3)
	if err == nil || !strings.Contains(err.Error(), "not found locally or on origin") {
		t.Fatalf("error = %v, want branch-not-found", err)
	}
}
