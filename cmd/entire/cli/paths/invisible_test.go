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
//	no               | enabled     | <git-common-dir>/entire/worktree/<worktree-key>/...
//	no               | off/absent  | <worktree>/.entire/...      (unchanged)
//
// The worktree key is HashWorktreeID over the git worktree identifier ("" for
// the main worktree), the same derivation shadow branch names use.
//
// No t.Parallel: these tests use t.Chdir and t.Setenv.
func TestAbsPath_InvisibleRouting_GloballyTracked(t *testing.T) {
	repo := newInvisibleTestRepo(t)
	setGlobalTier(t, `{"global":{"enabled":true}}`)

	base := filepath.Join(repo, ".git", "entire", "worktree", paths.HashWorktreeID(""))
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

// TestAbsPath_InvisibleRouting_ProbeFailure pins the fail-toward-invisibility
// contract: when the global tier owns the repo but the routing probe fails,
// AbsPath must ERROR for runtime-data paths (ErrUnroutableRuntimePath), never
// fall open into the worktree. Non-runtime paths keep resolving normally.
func TestAbsPath_InvisibleRouting_ProbeFailure(t *testing.T) {
	repo := newInvisibleTestRepo(t)
	setGlobalTier(t, `{"global":{"enabled":true}}`)
	paths.SetInvisibleProbeFailureForTesting(true)
	t.Cleanup(func() {
		paths.SetInvisibleProbeFailureForTesting(false)
		paths.ClearInvisibleRuntimeCache()
	})

	for _, rel := range []string{".entire/metadata/s1/prompt.txt", ".entire/logs", ".entire/tmp"} {
		_, err := paths.AbsPath(t.Context(), rel)
		if err == nil {
			t.Errorf("AbsPath(%q) succeeded; want ErrUnroutableRuntimePath", rel)
			continue
		}
		if !paths.IsUnroutableRuntimePath(err) {
			t.Errorf("AbsPath(%q) error %v does not carry ErrUnroutableRuntimePath", rel, err)
		}
	}

	// Non-runtime paths are unaffected by the probe failure.
	want := filepath.Join(repo, ".entire", "settings.json")
	if got := mustAbsPath(t, ".entire/settings.json"); got != want {
		t.Errorf("AbsPath(.entire/settings.json) = %q, want %q", got, want)
	}
	if got := mustAbsPath(t, "src/main.go"); got != filepath.Join(repo, "src", "main.go") {
		t.Errorf("AbsPath(src/main.go) = %q", got)
	}
}

// TestAbsPath_InvisibleRouting_SeparateGitDir pins that the common
// `git init --separate-git-dir` layout routes CORRECTLY (to the separate git
// dir) rather than failing: its .git-file gitdir matches no lexical worktree
// marker, which previously errored the worktree-ID probe and dropped routing
// into the worktree.
func TestAbsPath_InvisibleRouting_SeparateGitDir(t *testing.T) {
	repoDir, gitDir := initSeparateGitDirRepo(t)
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
	})
	setGlobalTier(t, `{"global":{"enabled":true}}`)

	want := filepath.Join(gitDir, "entire", "worktree", paths.HashWorktreeID(""), "logs")
	if got := mustAbsPath(t, ".entire/logs"); got != want {
		t.Errorf("AbsPath(.entire/logs) = %q, want separate-git-dir path %q", got, want)
	}
}

// TestAbsPath_InvisibleRouting_LinkedWorktree pins two properties of the
// routed base in a linked worktree: it lives under the git COMMON dir (the
// main repository's .git, not the per-worktree .git/worktrees/<name>/ dir),
// and it is ISOLATED per worktree — the linked worktree's base must differ
// from the main worktree's under the same common dir, so two globally
// tracked worktrees of one clone never interleave runtime data.
func TestAbsPath_InvisibleRouting_LinkedWorktree(t *testing.T) {
	repo := newInvisibleTestRepo(t)
	testutil.WriteFile(t, repo, "f.txt", "init")
	testutil.GitAdd(t, repo, "f.txt")
	testutil.GitCommit(t, repo, "init")
	setGlobalTier(t, `{"global":{"enabled":true}}`)

	mainLogs := mustAbsPath(t, ".entire/logs")

	linked := filepath.Join(repo, ".worktrees", "wt1")
	cmd := exec.CommandContext(t.Context(), "git", "worktree", "add", linked)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	t.Chdir(linked)
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()

	want := filepath.Join(repo, ".git", "entire", "worktree", paths.HashWorktreeID("wt1"), "logs")
	got := mustAbsPath(t, ".entire/logs")
	if got != want {
		t.Errorf("AbsPath(.entire/logs) = %q, want common-dir path %q", got, want)
	}
	if got == mainLogs {
		t.Errorf("linked worktree shares the main worktree's runtime base %q; worktree namespaces must be isolated", got)
	}
}
