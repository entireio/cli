package strategy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func newGlobalSetupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	return dir
}

func enableGlobalTier(t *testing.T) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setupRecord(t *testing.T) (repopolicy.SetupRecord, bool) {
	t.Helper()
	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := repopolicy.ReadSetupRecord(repository)
	if err != nil {
		t.Fatal(err)
	}
	return record, found
}

func primaryRefExists(t *testing.T, repoDir string) bool {
	t.Helper()
	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	_, err = repo.Reference(checkpoint.ResolveRefs(t.Context()).Primary, true)
	return err == nil
}

func TestMaybeEnsureGlobalSetup_RecordsComponentsPerWorktree(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	MaybeEnsureGlobalSetup(t.Context())
	if !IsGitHookInstalledInDir(t.Context(), repoDir) || !primaryRefExists(t, repoDir) {
		t.Fatal("lazy setup did not install hooks and primary ref")
	}
	record, found := setupRecord(t)
	if !found || record.GitHooksSpec != 1 || record.PrimaryRefSpec != 1 {
		t.Fatalf("setup record = %+v, found %v", record, found)
	}
}

func TestMaybeEnsureGlobalSetup_RepairsOnlyStaleComponent(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	MaybeEnsureGlobalSetup(t.Context())
	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := repopolicy.ReadSetupRecord(repository)
	if err != nil || !found {
		t.Fatalf("ReadSetupRecord() = (%+v, %v, %v)", record, found, err)
	}
	record.GitHooksSpec = 0
	if err := repopolicy.WriteSetupRecord(repository, record); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveGitHook(t.Context()); err != nil {
		t.Fatal(err)
	}
	MaybeEnsureGlobalSetup(t.Context())
	if !IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Fatal("stale git-hook component was not repaired")
	}
	repaired, _ := setupRecord(t)
	if repaired.GitHooksSpec != 1 || repaired.PrimaryRefSpec != 1 {
		t.Fatalf("repaired record = %+v", repaired)
	}
}

func TestMaybeEnsureGlobalSetup_RepairsStalePrimaryRefWithoutRewritingHooks(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	MaybeEnsureGlobalSetup(t.Context())
	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := repopolicy.ReadSetupRecord(repository)
	if err != nil || !found {
		t.Fatalf("ReadSetupRecord() = (%+v, %v, %v)", record, found, err)
	}
	record.PrimaryRefSpec = 0
	if err := repopolicy.WriteSetupRecord(repository, record); err != nil {
		t.Fatal(err)
	}
	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.RemoveReference(checkpoint.ResolveRefs(t.Context()).Primary); err != nil {
		repo.Close()
		t.Fatal(err)
	}
	repo.Close()
	hooksDir, err := GetHooksDir(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	before, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}

	MaybeEnsureGlobalSetup(t.Context())

	after, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("primary-ref repair rewrote an already-current git hook")
	}
	if !primaryRefExists(t, repoDir) {
		t.Fatal("stale primary-ref component was not repaired")
	}
	repaired, _ := setupRecord(t)
	if repaired.GitHooksSpec != 1 || repaired.PrimaryRefSpec != 1 {
		t.Fatalf("repaired record = %+v", repaired)
	}
}

func TestMaybeEnsureGlobalSetup_IsSilentWhenBackingUpUserHook(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	hookPath := filepath.Join(repoDir, ".git", "hooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho user-hook\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	restoreStderr := captureStderr(t)
	MaybeEnsureGlobalSetup(t.Context())
	if got := restoreStderr(); got != "" {
		t.Fatalf("lazy hook setup wrote to stderr: %q", got)
	}
	if _, err := os.Stat(hookPath + backupSuffix); err != nil {
		t.Fatalf("user hook backup missing: %v", err)
	}
}

func TestMaybeEnsureGlobalSetup_WorktreeHooksPathIsNeverMarkedInstalled(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	cmd := exec.CommandContext(t.Context(), "git", "config", "core.hooksPath", ".husky")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v\n%s", err, out)
	}
	ClearHooksDirCache()
	MaybeEnsureGlobalSetup(t.Context())
	record, found := setupRecord(t)
	if !found || record.GitHooksSpec != 0 || record.PrimaryRefSpec != 1 {
		t.Fatalf("unsafe hooksPath marked installed: %+v, found %v", record, found)
	}
}

func TestMaybeEnsureGlobalSetup_GlobalTierOff(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	MaybeEnsureGlobalSetup(t.Context())
	if IsGitHookInstalledInDir(t.Context(), repoDir) {
		t.Fatal("hooks installed while global tier off")
	}
	if _, found := setupRecord(t); found {
		t.Fatal("setup record written while global tier off")
	}
}

func TestEnsureSetupForHook_GloballyTracked_NoWorktreeWrites(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	enableGlobalTier(t)
	if err := EnsureSetupForHook(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(err) {
		t.Fatalf("hook setup wrote worktree .entire: %v", err)
	}
}

func TestEnsureSetupForHook_RepoEnabled_RunsEnsureSetup(t *testing.T) {
	repoDir := newGlobalSetupRepo(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true}`)
	if err := repopolicy.SetLocalActivation(t.Context(), repopolicy.ActivationEnabled); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSetupForHook(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".entire", ".gitignore")); err != nil {
		t.Fatalf("repo setup did not write .gitignore: %v", err)
	}
}
