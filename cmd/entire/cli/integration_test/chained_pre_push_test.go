//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// setupChainedPrePush installs a user pre-push hook that records it ran, then
// installs Entire's hooks through the real CLI so the user's hook is backed up
// and chained. The absolute hook path makes git run this build of entire rather
// than whatever is on PATH. Returns the marker the user's hook creates.
func setupChainedPrePush(t *testing.T, env *TestEnv) string {
	t.Helper()

	marker := filepath.Join(t.TempDir(), "user-pre-push-ran")
	hookPath := filepath.Join(env.RepoDir, ".git", "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("create hooks dir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write user pre-push: %v", err)
	}
	env.RunCLI("configure", "--absolute-git-hook-path")
	if _, err := os.Stat(hookPath + ".pre-entire"); err != nil {
		t.Fatalf("user pre-push should be backed up for chaining: %v", err)
	}
	return marker
}

// gitPushThroughInstalledHooks runs a plain `git push` with whatever hooks are
// installed. GitPushWithHooks is not used: it replaces the pre-push hook.
func gitPushThroughInstalledHooks(t *testing.T, env *TestEnv) error {
	t.Helper()

	cmd := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	output, err := cmd.CombinedOutput()
	t.Logf("git push output: %s", output)
	return err
}

// An OPF failure at pre-push must abort `git push` even when the user's own
// pre-push hook is chained after Entire's and would succeed.
func TestChainedPrePush_OPFFailureAbortsPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	marker := setupChainedPrePush(t, env)

	// A developer-owned OPF command that exits non-zero: OPF fails closed.
	env.WriteFile(".entire/opf", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(env.RepoDir, ".entire", "opf"), 0o755); err != nil {
		t.Fatal(err)
	}
	env.WriteFile(".entire/settings.local.json",
		`{"redaction":{"openai_privacy_filter":{"enabled":true,"prompt_default":"always",`+
			`"categories":{"private_person":true},"command":"./.entire/opf"}}}`)
	env.GitAdd(".entire/opf")
	env.GitCommit("Add local opf command")
	_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

	if err := gitPushThroughInstalledHooks(t, env); err == nil {
		t.Fatal("git push succeeded after OPF failed at pre-push; the chained user hook swallowed the failure")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("user pre-push ran after Entire's pre-push failed")
	}
}

// Positive control: with Entire's pre-push succeeding, the chained user hook
// runs and the push goes through.
func TestChainedPrePush_SuccessRunsUserHook(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	marker := setupChainedPrePush(t, env)
	_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

	if err := gitPushThroughInstalledHooks(t, env); err != nil {
		t.Fatalf("git push failed with Entire's pre-push succeeding: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("chained user pre-push did not run: %v", err)
	}
}
