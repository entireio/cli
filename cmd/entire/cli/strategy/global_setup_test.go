package strategy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestMaybeEnsureGlobalSetup_IncidentalSettingsInheritGlobal(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	testutil.WriteFile(t, repoDir, ".entire/settings.local.json", `{"investigate":{"max_turns":4}}`)
	paths.ClearInvisibleRuntimeCache()

	MaybeEnsureGlobalSetup(t.Context())

	if !IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Error("lazy setup did not run for a globally tracked repo with incidental settings")
	}
	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Error("clone preferences not marked after global lazy setup")
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

// TestMaybeEnsureGlobalSetup_PartialFailureRetries pins the marker's
// only-after-success contract: when part of the setup fails (here: an
// unwritable hooks dir), the marker must NOT be set, and the next hook
// activity retries and completes.
func TestMaybeEnsureGlobalSetup_PartialFailureRetries(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("chmod-based read-only directory is POSIX-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory write permissions are not enforced")
	}
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	ctx := t.Context()

	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	if err := os.Chmod(hooksDir, 0o500); err != nil {
		t.Fatalf("chmod hooks dir: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			if chmodErr := os.Chmod(hooksDir, 0o755); chmodErr != nil {
				t.Logf("restore hooks dir permissions: %v", chmodErr)
			}
			restored = true
		}
	}
	t.Cleanup(restore)

	MaybeEnsureGlobalSetup(ctx)

	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if prefs.GlobalSetupCompleted {
		t.Fatal("marker set despite hook install failure — partial failure latched as done")
	}

	restore()
	MaybeEnsureGlobalSetup(ctx)

	if !IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks not installed on retry")
	}
	prefs, err = settings.LoadClonePreferences(ctx)
	if err != nil {
		t.Fatalf("load clone preferences after retry: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Error("marker not set after successful retry")
	}
}

// TestMaybeEnsureGlobalSetup_ExcludedRepo pins the exclude-list interplay:
// with the tier on but the repo matching exclude_paths, the strict gate
// (settings.GlobalModeActive) keeps the lazy setup out entirely — no hooks,
// no ref, no marker.
//
// Documented acceptance: the invisible ROUTING probe in paths deliberately
// ignores exclude lists (see userGlobalTierEnabled — a strict superset of
// the strict gate), so a non-hook `entire` command run inside an excluded
// repo may still write its log under .git/entire/worktree/. That write is
// inside .git — invisible by construction — and is accepted.
func TestMaybeEnsureGlobalSetup_ExcludedRepo(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	userSettings := fmt.Sprintf(`{"global":{"enabled":true,"exclude_paths":[%q]}}`, repoDir)
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(userSettings), 0o600); err != nil {
		t.Fatalf("write user settings: %v", err)
	}
	ctx := t.Context()

	MaybeEnsureGlobalSetup(ctx)

	if IsGitHookInstalledInDir(ctx, repoDir) {
		t.Error("git hooks installed in an excluded repo")
	}
	if primaryRefExists(t, repoDir) {
		t.Error("primary metadata ref created in an excluded repo")
	}
	if _, err := os.Lstat(filepath.Join(repoDir, ".git", "entire", "preferences.json")); !os.IsNotExist(err) {
		t.Errorf("clone preferences created in an excluded repo (err=%v)", err)
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

func TestEnsureSetupForHook_IncidentalSettingsUseGlobalSetup(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	testutil.WriteFile(t, repoDir, ".entire/settings.local.json", `{"investigate":{"max_turns":4}}`)
	paths.ClearInvisibleRuntimeCache()

	if err := EnsureSetupForHook(t.Context()); err != nil {
		t.Fatalf("EnsureSetupForHook: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(repoDir, ".entire", ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("full repo setup ran for incidental settings (err=%v)", err)
	}
	if !IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Error("global lazy setup did not install git hooks")
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
