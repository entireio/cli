package review

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// defaultBranchName is the normalised default branch name used by initRepoOnMain.
const defaultBranchName = "main"

// openTestRepo opens a go-git repository from a directory.
func openTestRepo(t *testing.T, dir string) *git.Repository {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen(%s): %v", dir, err)
	}
	return repo
}

// commitFile creates, stages, and commits a new file in dir.
func commitFile(t *testing.T, dir, filename, content, message string) {
	t.Helper()
	testutil.WriteFile(t, dir, filename, content)
	testutil.GitAdd(t, dir, filename)
	testutil.GitCommit(t, dir, message)
}

// initRepoOnMain initializes a repo and ensures the default branch is "main".
// go-git's PlainInit creates "master" regardless of the host's
// init.defaultBranch setting; this helper normalises tests across environments
// by renaming via git CLI before the first commit.
func initRepoOnMain(t *testing.T, dir string) {
	t.Helper()
	testutil.InitRepo(t, dir)
	// Rename whatever go-git created (master) to "main". Works before any commits.
	//nolint:noctx // test helper
	cmd := exec.Command("git", "symbolic-ref", "HEAD", "refs/heads/main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set HEAD to main: %v\n%s", err, out)
	}
}

// TestFormatScopeBanner_Pluralisation verifies plural/singular forms.
func TestFormatScopeBanner_Pluralisation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stats ScopeStats
		want  string
	}{
		{
			name: "singular commit and file",
			stats: ScopeStats{
				CurrentBranch: "feat/x",
				BaseRef:       defaultBranchName,
				Commits:       1,
				FilesChanged:  1,
				Uncommitted:   0,
			},
			want: "Reviewing feat/x vs main: 1 commit, 1 file changed, 0 uncommitted",
		},
		{
			name: "plural commits and files",
			stats: ScopeStats{
				CurrentBranch: "feat/y",
				BaseRef:       defaultBranchName,
				Commits:       3,
				FilesChanged:  7,
				Uncommitted:   2,
			},
			want: "Reviewing feat/y vs main: 3 commits, 7 files changed, 2 uncommitted",
		},
		{
			name: "zero commits and files (plural for zero)",
			stats: ScopeStats{
				CurrentBranch: defaultBranchName,
				BaseRef:       defaultBranchName,
				Commits:       0,
				FilesChanged:  0,
				Uncommitted:   0,
			},
			want: "Reviewing main vs main: 0 commits, 0 files changed, 0 uncommitted",
		},
		{
			name: "detached HEAD",
			stats: ScopeStats{
				CurrentBranch: "",
				BaseRef:       defaultBranchName,
				Commits:       3,
				FilesChanged:  2,
				Uncommitted:   1,
			},
			want: "Reviewing detached HEAD vs main: 3 commits, 2 files changed, 1 uncommitted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatScopeBanner(tc.stats)
			if got != tc.want {
				t.Errorf("formatScopeBanner() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsAncestorOf tests the isAncestorOf helper with a real temp repo.
func TestIsAncestorOf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initRepoOnMain(t, dir)

	commitFile(t, dir, "a.go", "package main", "commit A")
	hashAStr := testutil.GetHeadHash(t, dir)

	commitFile(t, dir, "b.go", "package main", "commit B")
	hashBStr := testutil.GetHeadHash(t, dir)

	repo := openTestRepo(t, dir)
	ctx := context.Background()

	hashA := plumbing.NewHash(hashAStr)
	hashB := plumbing.NewHash(hashBStr)

	// A is an ancestor of B.
	isAnc, err := isAncestorOf(ctx, repo, hashA, hashB)
	if err != nil {
		t.Fatalf("isAncestorOf(A, B): %v", err)
	}
	if !isAnc {
		t.Error("A should be ancestor of B")
	}

	// B is NOT an ancestor of A.
	isAnc, err = isAncestorOf(ctx, repo, hashB, hashA)
	if err != nil {
		t.Fatalf("isAncestorOf(B, A): %v", err)
	}
	if isAnc {
		t.Error("B should not be ancestor of A")
	}

	// A is its own ancestor (equal hashes → true).
	isAnc, err = isAncestorOf(ctx, repo, hashA, hashA)
	if err != nil {
		t.Fatalf("isAncestorOf(A, A): %v", err)
	}
	if !isAnc {
		t.Error("A should be ancestor of itself (equal)")
	}
}

// TestDetectScopeBaseRef_BranchOffMain checks that a feature branch off main
// returns "main" and the commit/file counts are correct.
// Cannot use t.Parallel because it modifies disk state.
func TestDetectScopeBaseRef_BranchOffMain(t *testing.T) {
	dir := t.TempDir()
	initRepoOnMain(t, dir)

	// Create initial commit on main.
	commitFile(t, dir, "README.md", "hello", "init")

	// Create feat/x with 3 commits.
	testutil.GitCheckoutNewBranch(t, dir, "feat/x")
	commitFile(t, dir, "a.go", "package main", "add a.go")
	commitFile(t, dir, "b.go", "package main", "add b.go")
	commitFile(t, dir, "c.go", "package main", "add c.go")

	ctx := context.Background()
	repo := openTestRepo(t, dir)

	baseRef, err := detectScopeBaseRef(ctx, repo)
	if err != nil {
		t.Fatalf("detectScopeBaseRef: %v", err)
	}
	if baseRef != defaultBranchName {
		t.Errorf("baseRef = %q, want %q", baseRef, defaultBranchName)
	}

	// Verify commit count.
	commits, err := countCommits(ctx, dir, baseRef)
	if err != nil {
		t.Fatalf("countCommits: %v", err)
	}
	if commits != 3 {
		t.Errorf("commits = %d, want 3", commits)
	}

	// Verify files changed count.
	filesChanged, err := countFilesChanged(ctx, dir, baseRef)
	if err != nil {
		t.Fatalf("countFilesChanged: %v", err)
	}
	if filesChanged != 3 {
		t.Errorf("filesChanged = %d, want 3", filesChanged)
	}
}

// TestDetectScopeBaseRef_ClosestAncestorPreferred verifies that a branch
// stacked on top of another feature branch returns the immediate parent
// (more recent tip), not the more distant main.
// Cannot use t.Parallel because it modifies the repo state.
func TestDetectScopeBaseRef_ClosestAncestorPreferred(t *testing.T) {
	dir := t.TempDir()
	initRepoOnMain(t, dir)

	// main: one initial commit.
	commitFile(t, dir, "root.go", "package main", "init")

	// feat/parent: one commit off main.
	testutil.GitCheckoutNewBranch(t, dir, "feat/parent")
	commitFile(t, dir, "parent.go", "package main", "parent commit")

	// feat/child: two commits off feat/parent.
	testutil.GitCheckoutNewBranch(t, dir, "feat/child")
	commitFile(t, dir, "child1.go", "package main", "child commit 1")
	commitFile(t, dir, "child2.go", "package main", "child commit 2")

	ctx := context.Background()
	repo := openTestRepo(t, dir)

	baseRef, err := detectScopeBaseRef(ctx, repo)
	if err != nil {
		t.Fatalf("detectScopeBaseRef: %v", err)
	}
	// feat/parent has the most recent tip among ancestors of feat/child.
	if baseRef != "feat/parent" {
		t.Errorf("baseRef = %q, want %q", baseRef, "feat/parent")
	}
}

// TestDetectScopeBaseRef_DetachedHEAD verifies that detached HEAD falls
// back to the fallback chain.
// Cannot use t.Parallel because it modifies the repo state.
func TestDetectScopeBaseRef_DetachedHEAD(t *testing.T) {
	dir := t.TempDir()
	initRepoOnMain(t, dir)

	// Commit so we have a detachable commit.
	commitFile(t, dir, "file.go", "package main", "init")
	headSHA := testutil.GetHeadHash(t, dir)

	// Detach HEAD by checking out the SHA directly.
	//nolint:noctx // test helper
	cmd := exec.Command("git", "checkout", "--detach", headSHA)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detach HEAD: %v\n%s", err, out)
	}

	ctx := context.Background()
	repo := openTestRepo(t, dir)

	baseRef, err := detectScopeBaseRef(ctx, repo)
	if err != nil {
		t.Fatalf("detectScopeBaseRef: %v", err)
	}
	// With no ancestor branches (detached HEAD, no origin), falls back to "main".
	if baseRef != defaultBranchName {
		t.Errorf("baseRef = %q, want one of the fallback refs (e.g. %s)", baseRef, defaultBranchName)
	}

	// Banner should show "detached HEAD".
	stats := ScopeStats{
		BaseRef:       baseRef,
		CurrentBranch: "",
		Commits:       0,
		FilesChanged:  0,
		Uncommitted:   0,
	}
	banner := formatScopeBanner(stats)
	if !strings.Contains(banner, "detached HEAD") {
		t.Errorf("banner %q does not mention 'detached HEAD'", banner)
	}
}

// TestDetectScopeBaseRef_CleanDefaultBranch verifies that checking out main
// produces 0 commits vs itself.
// Cannot use t.Parallel because it modifies the repo state.
func TestDetectScopeBaseRef_CleanDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	initRepoOnMain(t, dir)

	// Single commit on main.
	commitFile(t, dir, "file.go", "package main", "init")

	ctx := context.Background()
	repo := openTestRepo(t, dir)

	baseRef, err := detectScopeBaseRef(ctx, repo)
	if err != nil {
		t.Fatalf("detectScopeBaseRef: %v", err)
	}

	commits, err := countCommits(ctx, dir, baseRef)
	if err != nil {
		t.Fatalf("countCommits: %v", err)
	}
	if commits != 0 {
		t.Errorf("commits = %d, want 0 (main vs itself)", commits)
	}

	stats := ScopeStats{
		BaseRef:       baseRef,
		CurrentBranch: currentBranchName(repo),
		Commits:       commits,
		FilesChanged:  0,
		Uncommitted:   0,
	}
	banner := formatScopeBanner(stats)
	wantBanner := "Reviewing main vs main: 0 commits, 0 files changed, 0 uncommitted"
	if banner != wantBanner {
		t.Errorf("banner = %q, want %q", banner, wantBanner)
	}
}

// TestDetectScopeBaseRef_UncommittedChanges verifies that uncommitted file
// changes are counted correctly.
// Cannot use t.Parallel because it modifies disk state.
func TestDetectScopeBaseRef_UncommittedChanges(t *testing.T) {
	dir := t.TempDir()
	initRepoOnMain(t, dir)

	// Initial commit on main.
	commitFile(t, dir, "tracked.go", "package main", "init")

	// Branch off main.
	testutil.GitCheckoutNewBranch(t, dir, "feat/dirty")

	// Modify a tracked file (not committed).
	testutil.WriteFile(t, dir, "tracked.go", "package main\n// modified")
	// Add an untracked file.
	testutil.WriteFile(t, dir, "untracked.go", "package main")

	ctx := context.Background()

	uncommitted, err := countUncommitted(ctx, dir)
	if err != nil {
		t.Fatalf("countUncommitted: %v", err)
	}
	if uncommitted != 2 {
		t.Errorf("uncommitted = %d, want 2 (1 modified tracked + 1 untracked)", uncommitted)
	}
}

// TestDetectScopeBaseRef_NoSuitableAncestor verifies that a fresh repo with
// no main/master/origin returns an error.
// Cannot use t.Parallel because it modifies the repo state.
func TestDetectScopeBaseRef_NoSuitableAncestor(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir) // intentionally NOT initRepoOnMain — we rename below

	// Commit so HEAD resolves (git init sets default branch to "master" or "main").
	commitFile(t, dir, "file.go", "package main", "init")

	// Determine which default branch was created.
	//nolint:noctx // test helper
	branchOut, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	defaultBranch := strings.TrimSpace(string(branchOut))

	// Rename default branch to a non-fallback name so detectScopeBaseRef
	// cannot resolve any fallback.
	//nolint:noctx // test helper
	cmd := exec.Command("git", "branch", "-m", defaultBranch, "custom-branch")
	cmd.Dir = dir
	if out, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
		t.Fatalf("rename branch: %v\n%s", cmdErr, out)
	}

	ctx := context.Background()

	// Re-open repo after rename.
	repo := openTestRepo(t, dir)

	_, detectErr := detectScopeBaseRef(ctx, repo)
	if detectErr == nil {
		t.Error("expected error when no suitable ancestor branch exists, got nil")
	}
}

// TestComputeScopeStats_Integration verifies the full ComputeScopeStats
// function produces consistent results.
// Cannot use t.Parallel because it modifies the filesystem.
func TestComputeScopeStats_Integration(t *testing.T) {
	dir := t.TempDir()
	initRepoOnMain(t, dir)

	// main: one commit.
	commitFile(t, dir, "main.go", "package main", "init")

	// feat/stats: 2 commits.
	testutil.GitCheckoutNewBranch(t, dir, "feat/stats")
	commitFile(t, dir, "x.go", "package main", "add x")
	commitFile(t, dir, "y.go", "package main", "add y")

	ctx := context.Background()
	repo := openTestRepo(t, dir)

	stats, err := ComputeScopeStats(ctx, repo)
	if err != nil {
		t.Fatalf("ComputeScopeStats: %v", err)
	}

	if stats.BaseRef != defaultBranchName {
		t.Errorf("BaseRef = %q, want %q", stats.BaseRef, defaultBranchName)
	}
	if stats.CurrentBranch != "feat/stats" {
		t.Errorf("CurrentBranch = %q, want %q", stats.CurrentBranch, "feat/stats")
	}
	if stats.Commits != 2 {
		t.Errorf("Commits = %d, want 2", stats.Commits)
	}
	if stats.FilesChanged != 2 {
		t.Errorf("FilesChanged = %d, want 2", stats.FilesChanged)
	}
	if stats.Uncommitted != 0 {
		t.Errorf("Uncommitted = %d, want 0", stats.Uncommitted)
	}
}
