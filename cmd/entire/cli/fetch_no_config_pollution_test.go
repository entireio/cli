package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestFetchDoesNotPolluteOriginConfig is a regression test for #712.
//
// Using `git fetch --filter=blob:none <remote-name>` causes git to persist
// remote.<name>.promisor=true and remote.<name>.partialclonefilter=blob:none
// into .git/config. That is sticky across future fetches on the same remote
// (including the user's regular `git fetch origin` / `git pull`), turning
// origin into a promisor remote and changing fetch behavior repo-wide.
//
// The fix fetches by URL instead of by remote name so git does not touch
// remote.origin.* in local config.
func TestFetchDoesNotPolluteOriginConfig(t *testing.T) {
	// Uses t.Chdir() — cannot run in parallel.

	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "bare.git")
	localDir := filepath.Join(tmpDir, "local")

	runGit(t, tmpDir, "init", "--bare", bareDir)

	// Set up local repo with an initial commit and the metadata branch pushed
	// to the bare remote.
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "hello")
	testutil.GitAdd(t, localDir, "README.md")
	testutil.GitCommit(t, localDir, "init")
	runGit(t, localDir, "remote", "add", "origin", bareDir)
	runGit(t, localDir, "branch", paths.MetadataBranchName)
	runGit(t, localDir, "push", "origin", "HEAD:refs/heads/main", paths.MetadataBranchName)
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	// Clone fresh so local has no metadata branch yet — this is the scenario
	// the fetch helpers in git_operations.go are designed for.
	clonedDir := filepath.Join(tmpDir, "cloned")
	runGit(t, tmpDir, "clone", "--branch", "main", bareDir, clonedDir)
	// git clone writes a global git config in some environments; configure
	// user identity so any subsequent ops here don't fail.
	runGit(t, clonedDir, "config", "user.email", "test@example.com")
	runGit(t, clonedDir, "config", "user.name", "Test")
	if err := os.MkdirAll(filepath.Join(clonedDir, ".entire"), 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	settingsJSON := `{"enabled": true, "strategy_options": {"filtered_fetches": true}}`
	if err := os.WriteFile(filepath.Join(clonedDir, ".entire", "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}

	t.Chdir(clonedDir)

	cases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"FetchMetadataBranch", FetchMetadataBranch},
		{"FetchMetadataTreeOnly", FetchMetadataTreeOnly},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear any previous pollution from earlier subtest runs. These
			// return exit 5 if the key is absent; ignore either outcome.
			//nolint:errcheck // cleanup is best-effort
			runGitAllow(t, clonedDir, "config", "--unset", "remote.origin.promisor")
			//nolint:errcheck // cleanup is best-effort
			runGitAllow(t, clonedDir, "config", "--unset", "remote.origin.partialclonefilter")

			if err := tc.fn(t.Context()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			if got := gitConfigValue(t, clonedDir, "remote.origin.promisor"); got != "" {
				t.Errorf("%s: remote.origin.promisor was set to %q — fetch leaked partial-clone config onto origin", tc.name, got)
			}
			if got := gitConfigValue(t, clonedDir, "remote.origin.partialclonefilter"); got != "" {
				t.Errorf("%s: remote.origin.partialclonefilter was set to %q — fetch leaked partial-clone config onto origin", tc.name, got)
			}
		})
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, output)
	}
}

func runGitAllow(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run()
}

func gitConfigValue(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "config", "--local", "--get", key)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.Output()
	if err != nil {
		// git config returns exit 1 when key is absent — treat as empty.
		return ""
	}
	return strings.TrimSpace(string(output))
}
