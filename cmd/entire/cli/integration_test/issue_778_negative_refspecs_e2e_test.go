//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestIssue778_NegativeRefspecs_RealBinaryOpensRepo is a full-flow reproduction
// of #778: a repo whose .git/config carries git 2.29+ negative (exclusion) fetch
// refspecs ("fetch = ^refs/...") could not be opened by go-git, whose refspec
// parser rejects the '^' form — so every entire command that opens the repo
// failed with "malformed refspec, separators are wrong".
//
// It drives the real entire binary end-to-end against such a repo (checkpoint
// rewind --list opens the repository via gitrepo.OpenPath) and asserts the
// command succeeds without the malformed-refspec error, while the on-disk config
// still carries the negative refspecs (native git needs them — the sanitizer
// only affects go-git's in-memory reads).
func TestIssue778_NegativeRefspecs_RealBinaryOpensRepo(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Build the repo with raw git rather than the go-git-backed harness helpers
	// (GitAdd/GitCommit would themselves fail to open once the negative refspecs
	// are present) and deliberately skip env.InitRepo(), whose .git/config guard
	// would flag the negative refspecs this test intentionally writes.
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = env.RepoDir
		cmd.Env = testutil.GitIsolatedEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "commit.gpgsign", "false")
	env.WriteFile("README.md", "# Test")
	runGit("add", "README.md")
	runGit("commit", "-m", "init")
	runGit("remote", "add", "origin", "git@github.com:example/example.git")

	// Append git 2.29+ negative fetch refspecs (valid for native git,
	// unparseable by go-git before the fix).
	cfgPath := filepath.Join(env.RepoDir, ".git", "config")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open .git/config: %v", err)
	}
	if _, err := f.WriteString("\tfetch = ^refs/heads/excluded\n\tfetch = ^refs/heads/other\n"); err != nil {
		t.Fatalf("append negative refspecs: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close .git/config: %v", err)
	}

	// Enable entire so the command proceeds to open the repository rather than
	// short-circuiting on the disabled check.
	env.InitEntire()

	// Drive the real binary: `checkpoint rewind --list` opens the repository.
	// Without the fix this fails with "malformed refspec, separators are wrong".
	out, err := env.RunCLIWithError("checkpoint", "rewind", "--list")
	if err != nil {
		t.Fatalf("entire failed to operate on a repo with negative refspecs (#778): %v\nOutput: %s", err, out)
	}
	if strings.Contains(out, "malformed refspec") {
		t.Fatalf("negative refspecs broke repository open (#778):\n%s", out)
	}

	// The on-disk config must be untouched so native git still honors the
	// negatives; the sanitizer only affects go-git's in-memory reads.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if !strings.Contains(string(raw), "^refs/heads/excluded") {
		t.Fatalf("sanitizer modified on-disk config; negative refspec removed:\n%s", raw)
	}
}
