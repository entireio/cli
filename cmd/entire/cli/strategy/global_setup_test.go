package strategy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newGlobalSetupRepo creates a temp repo with one commit, chdirs into it, and
// resets the paths caches so the invisible-routing decision is computed fresh.
func newGlobalSetupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
	})
	return dir
}

// enableGlobalTier writes a user-global settings file with global mode on
// into a fresh ENTIRE_CONFIG_DIR.
func enableGlobalTier(t *testing.T) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
		[]byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write user settings: %v", err)
	}
}

func primaryRefExists(t *testing.T, repoDir string) bool {
	t.Helper()
	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	_, err = repo.Reference(checkpoint.ResolveRefs(t.Context()).Primary, true)
	return err == nil
}

// No t.Parallel in this file: every test uses t.Chdir and t.Setenv.

func TestMaybeEnsureGlobalSetup_GloballyTracked(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	ctx := t.Context()

	MaybeEnsureGlobalSetup(ctx)

	if !IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks not installed")
	}
	if !primaryRefExists(t, repoDir) {
		t.Error("primary metadata ref not created")
	}
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Error("clone preferences not marked global_setup_completed")
	}
	// The invisible guarantee: setup must not create any worktree file.
	if _, err := os.Lstat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(err) {
		t.Errorf(".entire exists in worktree after global lazy setup (err=%v)", err)
	}

	// Idempotent second call.
	MaybeEnsureGlobalSetup(ctx)
	if !IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks missing after second call")
	}
}

// TestMaybeEnsureGlobalSetup_MarkerShortCircuits pins the "already done"
// contract: once the clone-prefs marker is set, the setup does no git work —
// even when the hooks are missing (e.g. removed by hand).
func TestMaybeEnsureGlobalSetup_MarkerShortCircuits(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	ctx := t.Context()

	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		p.GlobalSetupCompleted = true
		return nil
	}); err != nil {
		t.Fatalf("pre-mark clone preferences: %v", err)
	}

	MaybeEnsureGlobalSetup(ctx)

	if IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("marker set: setup should not have installed hooks")
	}
}

func TestMaybeEnsureGlobalSetup_GlobalTierOff(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir()) // no user settings file

	MaybeEnsureGlobalSetup(t.Context())

	if IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Error("git hooks installed although global tier is off")
	}
	if primaryRefExists(t, repoDir) {
		t.Error("primary metadata ref created although global tier is off")
	}
	if _, err := os.Lstat(filepath.Join(repoDir, ".git", "entire", "preferences.json")); !os.IsNotExist(err) {
		t.Errorf("clone preferences file created although global tier is off (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(err) {
		t.Errorf(".entire created although global tier is off (err=%v)", err)
	}
}

func TestMaybeEnsureGlobalSetup_RepoLevelSetupWins(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled": true}`)
	paths.ClearInvisibleRuntimeCache()

	MaybeEnsureGlobalSetup(t.Context())

	if IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Error("lazy setup ran in a repo-enabled repo (EnsureSetup owns it)")
	}
	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if prefs.GlobalSetupCompleted {
		t.Error("clone preferences marked global_setup_completed in a repo-enabled repo")
	}
}

// TestMaybeEnsureGlobalSetup_WorktreeHooksPath pins the hooksPath guard: when
// core.hooksPath resolves inside the worktree (e.g. a committed .husky dir),
// the lazy setup must not install hooks there — a worktree write would break
// invisibility. The rest of the setup still runs, and the marker IS set:
// worktree-resident hooksPath is a stable repo property, not a transient
// failure, and `entire doctor` is the surface that explains it.
func TestMaybeEnsureGlobalSetup_WorktreeHooksPath(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	gitCfg := exec.CommandContext(t.Context(), "git", "config", "core.hooksPath", ".husky")
	gitCfg.Dir = repoDir
	if out, err := gitCfg.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, out)
	}
	ClearHooksDirCache()
	t.Cleanup(ClearHooksDirCache)
	enableGlobalTier(t)
	ctx := t.Context()

	MaybeEnsureGlobalSetup(ctx)

	// No worktree writes: neither the hooks dir nor .entire may appear.
	if _, err := os.Lstat(filepath.Join(repoDir, ".husky")); !os.IsNotExist(err) {
		t.Errorf(".husky created in the worktree by lazy setup (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(err) {
		t.Errorf(".entire exists in worktree (err=%v)", err)
	}
	if IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks reported installed despite worktree-resident hooksPath")
	}
	// The ref half still ran, and the marker is set (documented choice).
	if !primaryRefExists(t, repoDir) {
		t.Error("primary metadata ref not created")
	}
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Error("marker not set: setup did everything it safely could and must not retry forever")
	}
}

// TestMaybeEnsureGlobalSetup_CorruptPreferences pins the repair path: a
// corrupt clone preferences file must not permanently kill the lazy setup.
// Setup treats it as fresh, proceeds, and the marker write recreates the
// file as valid JSON.
func TestMaybeEnsureGlobalSetup_CorruptPreferences(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	ctx := t.Context()

	prefsPath := filepath.Join(repoDir, ".git", "entire", "preferences.json")
	if err := os.MkdirAll(filepath.Dir(prefsPath), 0o750); err != nil {
		t.Fatalf("mkdir prefs dir: %v", err)
	}
	if err := os.WriteFile(prefsPath, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("write corrupt prefs: %v", err)
	}

	MaybeEnsureGlobalSetup(ctx)

	if !IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks not installed despite corrupt preferences (setup permanently wedged)")
	}
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		t.Fatalf("clone preferences not repaired: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Error("repaired clone preferences not marked global_setup_completed")
	}
}

func TestEnsureSetupForHook_GloballyTracked_NoWorktreeWrites(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	ctx := t.Context()

	if err := EnsureSetupForHook(ctx); err != nil {
		t.Fatalf("EnsureSetupForHook: %v", err)
	}

	// EnsureSetup would have written .entire/.gitignore; the hook-path variant
	// must not create anything in the worktree of a globally tracked repo.
	if _, err := os.Lstat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(err) {
		t.Errorf(".entire exists in worktree (err=%v)", err)
	}
	if !IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks not installed via hook-path setup")
	}
}

func TestEnsureSetupForHook_RepoEnabled_RunsEnsureSetup(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir()) // global tier irrelevant here
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled": true}`)
	paths.ClearInvisibleRuntimeCache()

	if err := EnsureSetupForHook(t.Context()); err != nil {
		t.Fatalf("EnsureSetupForHook: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".entire", ".gitignore")); err != nil {
		t.Errorf("EnsureSetup path did not write .entire/.gitignore: %v", err)
	}
	if !IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Error("git hooks not installed via EnsureSetup path")
	}
}
