package paths_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newInvisibleTestRepo creates a temp repo, chdirs into it, and resets the
// paths caches so each test's routing decision is computed fresh.
// Returns the symlink-resolved repo root (matching git's own output on macOS,
// where t.TempDir may return /var/... while git reports /private/var/...).
func newInvisibleTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
	})
	return dir
}

// setGlobalTier points ENTIRE_CONFIG_DIR at a temp dir containing the given
// user-global settings content. Empty content means "no settings file".
func setGlobalTier(t *testing.T, content string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if content != "" {
		if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(content), 0o600); err != nil {
			t.Fatalf("write user settings: %v", err)
		}
	}
}

func mustAbsPath(t *testing.T, rel string) string {
	t.Helper()
	got, err := paths.AbsPath(t.Context(), rel)
	if err != nil {
		t.Fatalf("AbsPath(%q): %v", rel, err)
	}
	return got
}

// The decision table: where does .entire runtime data live?
//
//	repo-level setup | global tier | runtime data location
//	-----------------+-------------+-----------------------------------
//	yes              | (ignored)   | <worktree>/.entire/...      (unchanged)
//	no               | enabled     | <git-common-dir>/entire/worktree/...
//	no               | off/absent  | <worktree>/.entire/...      (unchanged)
//
// No t.Parallel: these tests use t.Chdir and t.Setenv.
func TestAbsPath_InvisibleRouting_GloballyTracked(t *testing.T) {
	repo := newInvisibleTestRepo(t)
	setGlobalTier(t, `{"global":{"enabled":true}}`)

	base := filepath.Join(repo, ".git", "entire", "worktree")
	cases := map[string]string{
		".entire/metadata":                 filepath.Join(base, "metadata"),
		".entire/metadata/s1/prompt.txt":   filepath.Join(base, "metadata", "s1", "prompt.txt"),
		".entire/logs":                     filepath.Join(base, "logs"),
		".entire/tmp":                      filepath.Join(base, "tmp"),
		".entire/tmp/s1.json":              filepath.Join(base, "tmp", "s1.json"),
		".entire/settings.json":            filepath.Join(repo, ".entire", "settings.json"),
		".entire/settings.local.json":      filepath.Join(repo, ".entire", "settings.local.json"),
		".entire/.gitignore":               filepath.Join(repo, ".entire", ".gitignore"),
		".entire/metadata-lookalike/x":     filepath.Join(repo, ".entire", "metadata-lookalike", "x"),
		"src/main.go":                      filepath.Join(repo, "src", "main.go"),
		".entire/redactors/local/pack.yml": filepath.Join(repo, ".entire", "redactors", "local", "pack.yml"),
	}
	for rel, want := range cases {
		if got := mustAbsPath(t, rel); got != want {
			t.Errorf("AbsPath(%q) = %q, want %q", rel, got, want)
		}
	}
}

func TestAbsPath_InvisibleRouting_RepoLevelSetupWins(t *testing.T) {
	for _, settingsFile := range []string{"settings.json", "settings.local.json"} {
		t.Run(settingsFile, func(t *testing.T) {
			repo := newInvisibleTestRepo(t)
			setGlobalTier(t, `{"global":{"enabled":true}}`)
			testutil.WriteFile(t, repo, filepath.Join(".entire", settingsFile), `{"enabled": true}`)
			paths.ClearInvisibleRuntimeCache()

			want := filepath.Join(repo, ".entire", "metadata", "s1")
			if got := mustAbsPath(t, ".entire/metadata/s1"); got != want {
				t.Errorf("AbsPath = %q, want worktree path %q", got, want)
			}
		})
	}
}

func TestAbsPath_InvisibleRouting_GlobalTierOff(t *testing.T) {
	cases := map[string]string{
		"absent file": "",
		"disabled":    `{"global":{"enabled":false}}`,
		"no global":   `{}`,
		"malformed":   `{not json`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newInvisibleTestRepo(t)
			setGlobalTier(t, content)

			want := filepath.Join(repo, ".entire", "metadata", "s1")
			if got := mustAbsPath(t, ".entire/metadata/s1"); got != want {
				t.Errorf("AbsPath = %q, want worktree path %q", got, want)
			}
		})
	}
}

// TestAbsPath_InvisibleRouting_LinkedWorktree pins the routed base to the git
// COMMON dir: in a linked worktree, runtime data must land in the main
// repository's .git, not the per-worktree .git/worktrees/<name>/ dir.
func TestAbsPath_InvisibleRouting_LinkedWorktree(t *testing.T) {
	repo := newInvisibleTestRepo(t)
	testutil.WriteFile(t, repo, "f.txt", "init")
	testutil.GitAdd(t, repo, "f.txt")
	testutil.GitCommit(t, repo, "init")

	linked := filepath.Join(repo, ".worktrees", "wt1")
	cmd := exec.CommandContext(t.Context(), "git", "worktree", "add", linked)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	t.Chdir(linked)
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	setGlobalTier(t, `{"global":{"enabled":true}}`)

	want := filepath.Join(repo, ".git", "entire", "worktree", "logs")
	if got := mustAbsPath(t, ".entire/logs"); got != want {
		t.Errorf("AbsPath(.entire/logs) = %q, want common-dir path %q", got, want)
	}
}
