package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCheckpointRefZN = "refs/entire/checkpoints/ZN/01KVBJCWYA4YW6J5M9GP655HZN"

func TestFetchCheckpointRef_ElectionFailureCannotCertifyAbsence(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	originBare := t.TempDir()
	gitRun(t, originBare, "init", "--bare", "-q", originBare)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	gitRun(t, dir, "remote", "add", "origin", originBare)
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "gone")
	t.Chdir(dir)

	err := FetchCheckpointRef(context.Background(), plumbing.ReferenceName(testCheckpointRefZN))
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"fail-open origin cannot prove absence when checkpoint remote election failed")
}

// gitCheckout uses git CLI instead of go-git to work around go-git v5 bug
// where Checkout deletes untracked files (see https://github.com/go-git/go-git/issues/970).
func gitCheckout(t *testing.T, dir, ref string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "checkout", ref)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to checkout %s: %v\nOutput: %s", ref, err, output)
	}
}

func initOpenedTestRepo(t *testing.T, dir string) *git.Repository {
	t.Helper()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return repo
}

func TestValidateBranchNameRejectsLeadingDash(t *testing.T) {
	err := ValidateBranchName(context.Background(), "--all")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid branch name")
}

func TestGetCurrentBranch(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	// Create initial commit
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	// Create feature branch
	featureRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), commit)
	if err := repo.Storer.SetReference(featureRef); err != nil {
		t.Fatalf("Failed to create feature branch: %v", err)
	}

	// Checkout feature branch
	gitCheckout(t, tmpDir, "feature")

	// Test getting current branch
	branch, err := GetCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentBranch(context.Background()) error = %v", err)
	}
	if branch != "feature" {
		t.Errorf("GetCurrentBranch(context.Background()) = %v, want feature", branch)
	}
}

func TestGetCurrentBranchDetachedHead(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	// Create initial commit
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	// Checkout to detached HEAD
	gitCheckout(t, tmpDir, commit.String())

	// Test should error on detached HEAD
	_, err = GetCurrentBranch(context.Background())
	if err == nil {
		t.Error("GetCurrentBranch(context.Background()) expected error for detached HEAD, got nil")
	}
}

func TestHasUncommittedChanges(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	if _, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Test clean working tree
	hasChanges, err := HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = true, want false for clean tree")
	}

	// Make unstaged change
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Test with unstaged changes
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = false, want true for modified file")
	}

	// Stage the change
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}

	// Test with staged changes
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = false, want true for staged file")
	}

	// Commit and add untracked file
	if _, err := w.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("Failed to write untracked file: %v", err)
	}

	// Test with untracked file (should be true)
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = false, want true for untracked file")
	}

	// Clean up untracked file for next test
	if err := os.Remove(filepath.Join(tmpDir, "untracked.txt")); err != nil {
		t.Fatalf("Failed to remove untracked file: %v", err)
	}

	// Test global gitignore (core.excludesfile) handling
	// go-git doesn't read global gitignore, so we use git CLI instead.
	// Simulate global gitignore by setting core.excludesfile in repo config.
	// The file must be outside the repo to avoid showing up as untracked itself.
	globalIgnoreDir := t.TempDir()
	globalIgnoreFile := filepath.Join(globalIgnoreDir, "global-gitignore")
	if err := os.WriteFile(globalIgnoreFile, []byte("*.globally-ignored\n"), 0o644); err != nil {
		t.Fatalf("Failed to write global gitignore: %v", err)
	}

	// Set core.excludesfile in repo config
	cmd := exec.CommandContext(context.Background(), "git", "config", "core.excludesfile", globalIgnoreFile)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to set core.excludesfile: %v", err)
	}

	// Create a file that matches the global ignore pattern
	if err := os.WriteFile(filepath.Join(tmpDir, "secret.globally-ignored"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("Failed to write globally ignored file: %v", err)
	}

	// Test with globally gitignored file - should return false (clean)
	// This catches regressions if someone switches back to go-git's Status()
	// which doesn't read core.excludesfile (global gitignore)
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = true, want false for globally gitignored file (core.excludesfile)")
	}
}

func TestGetGitConfigValue(t *testing.T) {
	// Test that invalid keys return empty string
	invalid := getGitConfigValue(context.Background(), "nonexistent.key.that.does.not.exist")
	if invalid != "" {
		t.Errorf("expected empty string for invalid key, got %q", invalid)
	}

	// Test that it returns a value for user.name (assuming git is configured on test machine)
	// This is a basic sanity check - it may return empty on unconfigured systems
	name := getGitConfigValue(context.Background(), "user.name")
	t.Logf("git config user.name returned: %q", name)
}

func TestGetGitConfigValueTrimsWhitespace(t *testing.T) {
	// The git config command returns values with trailing newline
	// Verify that getGitConfigValue trims whitespace properly
	email := getGitConfigValue(context.Background(), "user.email")
	t.Logf("git config user.email returned: %q", email)

	// If email is set, verify no leading/trailing whitespace
	if email != "" {
		if email[0] == ' ' || email[0] == '\n' || email[0] == '\t' {
			t.Errorf("expected no leading whitespace, got %q", email)
		}
		if email[len(email)-1] == ' ' || email[len(email)-1] == '\n' || email[len(email)-1] == '\t' {
			t.Errorf("expected no trailing whitespace, got %q", email)
		}
	}
}

func TestGetGitAuthorReturnsAuthor(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo with user config
	repo := initOpenedTestRepo(t, tmpDir)

	// Set local user config
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("Failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test Author"
	cfg.User.Email = "test@example.com"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("Failed to set repo config: %v", err)
	}

	// Test GetGitAuthor
	author, err := GetGitAuthor(context.Background())
	if err != nil {
		t.Fatalf("GetGitAuthor(context.Background()) error = %v", err)
	}

	if author.Name != "Test Author" {
		t.Errorf("GetGitAuthor(context.Background()).Name = %q, want %q", author.Name, "Test Author")
	}
	if author.Email != "test@example.com" {
		t.Errorf("GetGitAuthor(context.Background()).Email = %q, want %q", author.Email, "test@example.com")
	}
}

func TestGetGitAuthorFallsBackToGitCommand(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo WITHOUT setting user config in go-git
	// This simulates the case where go-git can't find the config
	_, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	// GetGitAuthor should NOT error - it falls back to git command or returns defaults
	author, err := GetGitAuthor(context.Background())
	if err != nil {
		t.Fatalf("GetGitAuthor(context.Background()) should not error, got: %v", err)
	}

	// Verify it's not nil first
	require.NotNil(t, author, "GetGitAuthor(context.Background()) returned nil author")

	// The author should have some value (either from global git config or defaults)
	t.Logf("GetGitAuthor(context.Background()) returned Name=%q, Email=%q", author.Name, author.Email)
}

func TestGetGitAuthorReturnsDefaultsWhenNoConfig(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo without user config
	_, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	// Even without config, GetGitAuthor should not error
	// It will return either values from global git config OR defaults
	author, err := GetGitAuthor(context.Background())
	if err != nil {
		t.Fatalf("GetGitAuthor(context.Background()) should not error even without config, got: %v", err)
	}

	// Just verify we got a non-nil result first
	require.NotNil(t, author, "GetGitAuthor(context.Background()) returned nil")

	// Name and Email should be non-empty (either from global config or defaults)
	if author.Name == "" {
		t.Error("GetGitAuthor(context.Background()).Name is empty, expected a value or default")
	}
	if author.Email == "" {
		t.Error("GetGitAuthor(context.Background()).Email is empty, expected a value or default")
	}
}

func TestBranchExistsOnRemote(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Create remote reference (simulating a pushed branch)
	remoteRef := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "feature"), commit)
	if err := repo.Storer.SetReference(remoteRef); err != nil {
		t.Fatalf("Failed to create remote ref: %v", err)
	}

	t.Run("returns true when branch exists on remote", func(t *testing.T) {
		exists, err := BranchExistsOnRemote(context.Background(), "feature")
		if err != nil {
			t.Fatalf("BranchExistsOnRemote(context.Background(),) error = %v", err)
		}
		if !exists {
			t.Error("BranchExistsOnRemote(context.Background(),) = false, want true for existing remote branch")
		}
	})

	t.Run("returns false when branch does not exist on remote", func(t *testing.T) {
		exists, err := BranchExistsOnRemote(context.Background(), "nonexistent")
		if err != nil {
			t.Fatalf("BranchExistsOnRemote(context.Background(),) error = %v", err)
		}
		if exists {
			t.Error("BranchExistsOnRemote(context.Background(),) = true, want false for nonexistent remote branch")
		}
	})
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointFetchTarget_NoCheckpointRemote(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add origin remote
	testutil.RunGit(t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")

	// Settings with no checkpoint_remote
	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true}`),
		0o644,
	))

	t.Chdir(localDir)

	target := resolveCheckpointFetchTarget(context.Background())
	assert.Equal(t, "git@github.com:org/main-repo.git", target)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointFetchTarget_WithCheckpointRemote(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add SSH origin remote — checkpoint URL derives protocol from origin
	testutil.RunGit(t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")

	// Settings with checkpoint_remote configured
	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	target := resolveCheckpointFetchTarget(context.Background())
	assert.Equal(t, "git@github.com:org/checkpoints.git", target)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointFetchTarget_FallsBackOnError(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// No origin remote — FetchURL cannot resolve an effective fetch URL.

	// Settings with checkpoint_remote configured but no origin to derive URL from
	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	// Falls back to the origin remote name when URL resolution fails.
	target := resolveCheckpointFetchTarget(context.Background())
	assert.Equal(t, "origin", target)
}

// setupRepoWithBlobOnMetadataBranch creates a repo with a blob committed on
// entire/checkpoints/v1, checks out the default branch, and returns
// (repoDir, blobHash) for tests that need a reachable blob on the metadata branch.
func setupRepoWithBlobOnMetadataBranch(t *testing.T) (string, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	defaultBranch := gitDefaultBranch(t, dir)

	gitRun(t, dir, "checkout", "--orphan", "entire/checkpoints/v1")
	gitRun(t, dir, "rm", "-rf", ".")
	testutil.WriteFile(t, dir, "ab/cdef123456/metadata.json", `{"checkpoint_id": "abcdef123456"}`)
	testutil.GitAdd(t, dir, "ab/cdef123456/metadata.json")
	gitRun(t, dir, "-c", "commit.gpgsign=false", "commit", "-m", "Checkpoint: abcdef123456")

	blobHash := plumbing.NewHash(gitOutput(t, dir, "rev-parse", "HEAD:ab/cdef123456/metadata.json"))

	gitRun(t, dir, "checkout", defaultBranch)
	return dir, blobHash
}

// Not parallel: uses t.Chdir()
// Tests blob hydration from the normal target and from the legacy tier after a
// slow elected target exhausts only its own budget.
func TestFetchBlobsByHash_FetchesMissingBlob(t *testing.T) {
	ctx := context.Background()

	remoteDir, blobHash := setupRepoWithBlobOnMetadataBranch(t)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Origin is the remote that has the blob. With no checkpoint_remote
	// configured, resolveCheckpointFetchTarget returns "origin".
	gitRun(t, localDir, "remote", "add", "origin", remoteDir)

	t.Chdir(localDir)

	// Precondition: blob is not local
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	require.Error(t, localRepo.Storer.HasEncodedObject(blobHash), "blob should not exist locally before fetch")

	// Fetch succeeds; blob lands in local store
	require.NoError(t, FetchBlobsByHash(ctx, []plumbing.Hash{blobHash}))

	freshRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	require.NoError(t, freshRepo.Storer.HasEncodedObject(blobHash), "blob should exist locally after fetch")

	upstreamDir := t.TempDir()
	testutil.InitRepo(t, upstreamDir)
	originDir := t.TempDir()
	testutil.InitRepo(t, originDir)

	fallbackDir := t.TempDir()
	testutil.InitRepo(t, fallbackDir)
	testutil.WriteFile(t, fallbackDir, "f.txt", "init")
	testutil.GitAdd(t, fallbackDir, "f.txt")
	testutil.GitCommit(t, fallbackDir, "init")
	gitRun(t, fallbackDir, "remote", "add", "upstream", upstreamDir)
	gitRun(t, fallbackDir, "remote", "add", "origin", originDir)
	testutil.WriteCheckpointPushRemoteSetting(t, fallbackDir, "upstream")
	t.Chdir(fallbackDir)
	require.Equal(t, []string{upstreamDir, originDir}, checkpointBlobFetchTargets(ctx))

	var attempted []string
	fetch := func(candidateCtx context.Context, target string, _ []string) error {
		attempted = append(attempted, target)
		if target == upstreamDir {
			<-candidateCtx.Done()
			return candidateCtx.Err()
		}
		return candidateCtx.Err()
	}
	require.NoError(t, fetchBlobsByHash(ctx, []plumbing.Hash{blobHash}, 10*time.Millisecond, time.Second, fetch))
	require.Equal(t, []string{upstreamDir, originDir}, attempted)
}

// Not parallel: uses t.Chdir()
// Tests that FetchBlobsByHash returns an error when the blob is unreachable
// from the resolved target and both fallback fetches fail.
func TestFetchBlobsByHash_FailsWhenBlobUnreachable(t *testing.T) {
	ctx := context.Background()

	// Origin has no metadata branch, no blobs
	originDir := t.TempDir()
	testutil.InitRepo(t, originDir)
	testutil.WriteFile(t, originDir, "f.txt", "init")
	testutil.GitAdd(t, originDir, "f.txt")
	testutil.GitCommit(t, originDir, "init")

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	gitRun(t, localDir, "remote", "add", "origin", originDir)

	t.Chdir(localDir)

	// Arbitrary hash nobody has
	unreachable := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	err := FetchBlobsByHash(ctx, []plumbing.Hash{unreachable})
	require.Error(t, err, "FetchBlobsByHash should fail when blob is unreachable and no fallback succeeds")
}

// gitRun runs a git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	testutil.RunGit(t, dir, args...)
}

// gitOutput runs a git command and returns trimmed stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, dir, args...))
}

// gitDefaultBranch returns the current branch name in a repo.
func gitDefaultBranch(t *testing.T, dir string) string {
	t.Helper()
	return gitOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// TestParseCheckpointRefNames verifies the ls-remote parser keeps only
// checkpoint refs and ignores unrelated advertisement lines (HEAD, branches,
// peeled tags) and blanks.
func TestParseCheckpointRefNames(t *testing.T) {
	t.Parallel()
	const sha = "e9ed0bd3ad3b2071aefab6e6ad20527dc910957b"
	output := []byte(strings.Join([]string{
		sha + "\tHEAD",
		sha + "\trefs/heads/main",
		sha + "\t" + testCheckpointRefZN,
		sha + "\trefs/entire/checkpoints/f6/a1b2c3d4e5f6",
		sha + "\trefs/tags/v1.0.0",
		sha + "\trefs/tags/v1.0.0^{}",
		"",
	}, "\n"))

	names := parseCheckpointRefNames(output)
	got := make([]string, len(names))
	for i, n := range names {
		got[i] = n.String()
	}
	assert.ElementsMatch(t, []string{
		testCheckpointRefZN,
		"refs/entire/checkpoints/f6/a1b2c3d4e5f6",
	}, got)
}

// TestParseCheckpointRefNames_RealLsRemote exercises the parser against genuine
// `git ls-remote 'refs/entire/checkpoints/*'` output from a local bare remote,
// confirming the glob matches the nested <shard>/<id> refs and nothing else.
func TestParseCheckpointRefNames_RealLsRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	bareDir := t.TempDir()
	gitRun(t, bareDir, "init", "--bare", "-q", bareDir)

	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "init")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	head := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitRun(t, workDir, "update-ref", testCheckpointRefZN, head)
	gitRun(t, workDir, "update-ref", "refs/entire/checkpoints/f6/a1b2c3d4e5f6", head)
	gitRun(t, workDir, "remote", "add", "origin", bareDir)
	gitRun(t, workDir, "push", "-q", "origin", "refs/entire/checkpoints/*:refs/entire/checkpoints/*")

	out, err := remote.LsRemoteInDir(ctx, workDir, bareDir, checkpoint.CheckpointRefPrefix+"*")
	require.NoError(t, err)

	names := parseCheckpointRefNames(out)
	got := make([]string, len(names))
	for i, n := range names {
		got[i] = n.String()
	}
	assert.ElementsMatch(t, []string{
		testCheckpointRefZN,
		"refs/entire/checkpoints/f6/a1b2c3d4e5f6",
	}, got)
}

// TestListCheckpointRefsOnRemote_NotConfigured: with no checkpoint_remote and
// no git remotes at all (empty read-candidate chain), enumeration is a no-op
// (nil, no error, no network) so List stays local-only.
// Not parallel: uses t.Chdir.
func TestListCheckpointRefsOnRemote_NotConfigured(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	names, err := ListCheckpointRefsOnRemote(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names, "a remoteless repo must leave List local-only (no remote enumeration)")

	bareDir := t.TempDir()
	gitRun(t, bareDir, "init", "--bare", "-q", bareDir)
	head := gitOutput(t, dir, "rev-parse", "HEAD")
	ref := testCheckpointRefZN
	gitRun(t, dir, "remote", "add", "origin", bareDir)
	gitRun(t, dir, "push", "-q", "origin", head+":"+ref)
	testutil.WriteFile(t, dir, ".entire/settings.json",
		`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github"}}}`)

	names, err = ListCheckpointRefsOnRemote(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names, "a malformed checkpoint_remote must not activate candidate discovery")
}

// TestListCheckpointRefsOnRemote_MergesReadCandidateListings: without a
// dedicated checkpoint_remote, discovery ls-remotes every read candidate and
// merges the listings — a union deduped by ref name. Refs are seeded
// DISJOINTLY (one on the elected upstream, one on legacy origin, one on both)
// to pin that merging, not first-non-empty, is the semantics.
// Not parallel: uses t.Chdir.
func TestListCheckpointRefsOnRemote_MergesReadCandidateListings(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	originBare := t.TempDir()
	upstreamBare := t.TempDir()
	gitRun(t, originBare, "init", "--bare", "-q", originBare)
	gitRun(t, upstreamBare, "init", "--bare", "-q", upstreamBare)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	head := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "remote", "add", "origin", originBare)
	gitRun(t, dir, "remote", "add", "upstream", upstreamBare)
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")

	upstreamRef := testCheckpointRefZN
	originRef := "refs/entire/checkpoints/f6/a1b2c3d4e5f6"
	sharedRef := "refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9"
	gitRun(t, dir, "push", "-q", "upstream", head+":"+upstreamRef, head+":"+sharedRef)
	gitRun(t, dir, "push", "-q", "origin", head+":"+originRef, head+":"+sharedRef)
	t.Chdir(dir)

	names, err := ListCheckpointRefsOnRemote(context.Background())
	require.NoError(t, err)
	got := make([]string, len(names))
	for i, n := range names {
		got[i] = n.String()
	}
	assert.ElementsMatch(t, []string{upstreamRef, originRef, sharedRef}, got,
		"discovery must union the candidates' listings and dedupe by ref name")
}

// TestListCheckpointRefsOnRemote_CandidateFailureDoesNotBlockOthers: discovery
// is best-effort — an unreachable elected remote logs and continues, so the
// legacy origin tier's refs are still discovered. When EVERY candidate fails,
// an error restores the store's local-only warning.
// Not parallel: uses t.Chdir.
func TestListCheckpointRefsOnRemote_CandidateFailureDoesNotBlockOthers(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	originBare := t.TempDir()
	gitRun(t, originBare, "init", "--bare", "-q", originBare)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	head := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "remote", "add", "origin", originBare)
	gitRun(t, dir, "remote", "add", "upstream", filepath.Join(dir, "nonexistent-remote"))
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")

	originRef := "refs/entire/checkpoints/f6/a1b2c3d4e5f6"
	gitRun(t, dir, "push", "-q", "origin", head+":"+originRef)
	t.Chdir(dir)

	names, err := ListCheckpointRefsOnRemote(context.Background())
	require.NoError(t, err, "one candidate failing must not fail discovery")
	require.Len(t, names, 1)
	assert.Equal(t, originRef, names[0].String())

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	gitRun(t, dir, "remote", "set-url", "upstream", server.URL+"/repo.git")

	names, err = listCheckpointRefsOnRemote(context.Background(), time.Second)
	require.NoError(t, err, "a slow candidate must not consume origin's discovery budget")
	require.Len(t, names, 1)
	assert.Equal(t, originRef, names[0].String())

	gitRun(t, dir, "remote", "set-url", "upstream", filepath.Join(dir, "nonexistent-remote"))
	gitRun(t, dir, "remote", "set-url", "origin", filepath.Join(dir, "also-nonexistent"))
	names, err = ListCheckpointRefsOnRemote(context.Background())
	require.Error(t, err, "all candidates failing must trigger the store's local-only warning")
	assert.Nil(t, names)
}

// TestListCheckpointRefsOnRemote_ResolvesFromSubdir proves worktree pinning for
// the configured path: settings + ls-remote run from the worktree root even when
// process cwd is a subdirectory. Uses a local bare remote and an unknown
// checkpoint_remote provider so FetchURL falls back to origin (offline).
// Not parallel: uses t.Chdir.
func TestListCheckpointRefsOnRemote_ResolvesFromSubdir(t *testing.T) {
	bareDir := t.TempDir()
	gitRun(t, bareDir, "init", "--bare", "-q", bareDir)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	head := gitOutput(t, dir, "rev-parse", "HEAD")
	ref := testCheckpointRefZN
	gitRun(t, dir, "update-ref", ref, head)
	gitRun(t, dir, "remote", "add", "origin", bareDir)
	gitRun(t, dir, "push", "-q", "origin", ref+":"+ref)

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	// provider "local" is unknown to providerHost → FetchURL falls back to origin
	// (the bare path) after file:// derivation fails — keeps this offline.
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"local","repo":"org/checkpoints"}}}`),
		0o644,
	))

	sub := filepath.Join(dir, "nested", "deep")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	t.Chdir(sub)

	names, err := ListCheckpointRefsOnRemote(context.Background())
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.Equal(t, ref, names[0].String())
}

// TestListCheckpointRefsOnRemote_HonorsCanceledContext proves the discovery
// timeout context reaches the git subprocess: an already-canceled ctx with a
// configured checkpoint_remote must error (not hang / not silently succeed).
// Not parallel: uses t.Chdir.
func TestListCheckpointRefsOnRemote_HonorsCanceledContext(t *testing.T) {
	bareDir := t.TempDir()
	gitRun(t, bareDir, "init", "--bare", "-q", bareDir)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	gitRun(t, dir, "remote", "add", "origin", bareDir)

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"local","repo":"org/checkpoints"}}}`),
		0o644,
	))
	t.Chdir(dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ListCheckpointRefsOnRemote(ctx)
	require.Error(t, err, "canceled context must reach ls-remote (regression: deleting WithTimeout would still pass a constant-only test)")
}

// TestFetchBlobsByHash_ChainBudgetBoundsTheWholeOperation pins the ceiling that
// per-target budgets alone do not give. Before the read-candidate chain this
// function was wrapped in one 2-minute budget covering its fallbacks; per-target
// budgets replaced it, so worst-case latency scaled with the target count and the
// fallback metadata chain ran on the caller's uncapped context on top of that.
//
// Every target here stalls until its context expires, so without the ceiling the
// elapsed time would be at least perTarget × len(targets).
func TestFetchBlobsByHash_ChainBudgetBoundsTheWholeOperation(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir modifies process-global state.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "fork", "https://example.com/fork.git")
	// Elect a non-origin remote so the read chain is genuinely two tiers
	// (elected, then the legacy origin tier). With origin elected the chain
	// collapses to one candidate and the loop's worst case is a single window —
	// nothing for a ceiling to bind against.
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "fork")
	t.Chdir(dir)

	// Production-shaped ratio, scaled down: the ceiling sits BELOW the loop's own
	// worst case (targets x perTarget), which is the whole point — sized at or
	// above it the ceiling cannot bind and buys nothing. That was the original
	// defect here, and the first version of this test hid it by inverting the
	// relationship (perTarget far larger than the ceiling), which proves only that
	// some ceiling can bind, not that this one does.
	const perTarget = 400 * time.Millisecond
	const chainBudget = 500 * time.Millisecond // < 2 targets x perTarget

	var attempts int
	stall := func(ctx context.Context, _ string, _ []string) error {
		attempts++
		<-ctx.Done() // hang until this attempt's budget expires
		return ctx.Err()
	}

	start := time.Now()
	err := fetchBlobsByHash(t.Context(), []plumbing.Hash{plumbing.NewHash(
		"1111111111111111111111111111111111111111")}, perTarget, chainBudget, stall)
	elapsed := time.Since(start)

	require.Error(t, err, "every target stalled, so hydration cannot succeed")
	assert.GreaterOrEqual(t, attempts, 1, "at least one target must be attempted")

	// The ceiling, not targets x perTarget, decides how long the user waits. The
	// bound has to be below the loop's own worst case or it proves nothing;
	// slack on top absorbs a loaded CI box.
	targetCount := len(checkpointBlobFetchTargets(t.Context()))
	require.GreaterOrEqual(t, targetCount, 2,
		"setup: the ceiling can only be shown to bind against a multi-candidate chain")
	loopWorstCase := time.Duration(targetCount) * perTarget
	assert.Less(t, elapsed, chainBudget+300*time.Millisecond,
		"the chain ceiling must bound the whole operation including fallbacks (elapsed %s)", elapsed)
	assert.Less(t, elapsed, loopWorstCase,
		"a ceiling at or above the loop's worst case (%s) cannot bind — that was the original defect", loopWorstCase)
}
