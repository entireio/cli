//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// legacyGitHookEras are the two shapes local-dev mode wrote into .git/hooks over
// time. Both ran Entire from the working tree; neither is generated any more.
//
// The argument suffixes are reproduced verbatim from what those versions emitted,
// because they are what makes the two eras behave differently after an upgrade:
// every `go run` hook carried `|| true`, while the launcher-era pre-push did not
// (a repo-relative prefix got no availability guard, and pre-push deliberately
// propagates exit codes for OPF). So the launcher era fails a push outright,
// where the `go run` era merely keeps working slowly.
var legacyGitHookEras = []struct {
	name    string
	command func(hookName, args string) string
}{
	{
		name: "go run era",
		command: func(hookName, args string) string {
			return "go run ./cmd/entire/main.go hooks git " + hookName + " " + args + " || true"
		},
	},
	{
		name: "entire-dev launcher era",
		command: func(hookName, args string) string {
			// Deliberately unguarded and without `|| true`, matching what the
			// pre-removal generator produced for a repo-relative prefix.
			return "./scripts/entire-dev hooks git " + hookName + " " + args
		},
	},
}

// legacyHookArgs mirrors the per-hook argument lists in strategy.buildHookSpecs.
var legacyHookArgs = map[string]string{
	"prepare-commit-msg": `"$1" "$2" 2>/dev/null`,
	"commit-msg":         `"$1"`,
	"post-commit":        `2>/dev/null`,
	"post-rewrite":       `"$1" 2>/dev/null`,
	"pre-push":           `"$1"`,
}

// TestLocalDevMigration_TurnStartReplacesLegacyGitHooks covers the upgrade path
// for a clone that still has local-dev git hooks.
//
// `.git/hooks/*` are per-clone and untracked, so pulling the release that removed
// local-dev mode does not touch them — they keep naming a launcher inside the
// working tree, which by then is either deleted or merely slow. They also still
// carry Entire's marker, so a marker-only check reports them as installed and
// nothing ever replaces them. This pins that the first turn-start migrates them
// instead, since that is the only thing a user does without being told to.
func TestLocalDevMigration_TurnStartReplacesLegacyGitHooks(t *testing.T) {
	t.Parallel()

	for _, era := range legacyGitHookEras {
		t.Run(era.name, func(t *testing.T) {
			t.Parallel()

			env := NewRepoWithCommit(t)
			hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
			seedLegacyGitHooks(t, hooksDir, era.command)

			// The trap this guards: legacy hooks carry the marker, so anything
			// checking only for that would call them installed and skip the
			// reinstall forever.
			if strategy.IsGitHookInstalledInDir(context.Background(), env.RepoDir) {
				t.Fatal("legacy local-dev hooks must not count as installed, or EnsureSetup never migrates them")
			}

			session := env.NewSession()
			if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}

			if !strategy.IsGitHookInstalledInDir(context.Background(), env.RepoDir) {
				t.Error("hooks should read as installed after the migrating turn-start")
			}
			assertGitHooksInvokeBinary(t, hooksDir)
		})
	}
}

// TestLocalDevMigration_LauncherEraPushIsRepaired pins the user-visible symptom
// of the launcher era specifically: its pre-push hook was generated with no
// availability guard and no `|| true`, so once the tracked launcher script is
// gone (the upgrade deletes it) the hook fails and git rejects the push. The
// migration has to actually clear that, not just rewrite the file.
func TestLocalDevMigration_LauncherEraPushIsRepaired(t *testing.T) {
	t.Parallel()

	env := NewRepoWithCommit(t)
	// SetupBareRemote pushes the current HEAD, so add a commit afterwards:
	// git skips pre-push entirely when there is nothing to send, which would
	// make this test pass without ever running the hook. GitCommit goes through
	// go-git and fires no hooks, so it cannot trip the broken hook itself.
	env.SetupBareRemote()
	env.WriteFile("migration.txt", "needs pushing\n")
	env.GitAdd("migration.txt")
	env.GitCommit("add something to push")

	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	seedLegacyGitHooks(t, hooksDir, func(hookName, args string) string {
		return "./scripts/entire-dev hooks git " + hookName + " " + args
	})

	// The upgrade removed scripts/entire-dev, so the hook now names a path that
	// does not exist. Confirm that genuinely breaks the push before asserting the
	// repair — otherwise this test could pass without ever reproducing the bug.
	if out, err := pushWithHooksExpectingResult(env, "origin", "HEAD"); err == nil {
		t.Fatalf("expected the push to fail while a legacy unguarded pre-push hook names a missing launcher, got:\n%s", out)
	}

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	assertGitHooksInvokeBinary(t, hooksDir)

	if out, err := pushWithHooksExpectingResult(env, "origin", "HEAD"); err != nil {
		t.Errorf("push should succeed after migration, got %v\noutput: %s", err, out)
	}
}

// pushWithHooksExpectingResult pushes WITHOUT --no-verify so the installed
// pre-push hook runs, returning the outcome instead of failing the test. The
// harness's GitPushWithHooks fatals on error and reinstalls its own pre-push
// hook, both of which defeat the point here.
func pushWithHooksExpectingResult(env *TestEnv, remote, refSpec string) (string, error) {
	env.T.Helper()

	cmd := execx.NonInteractive(env.T.Context(), "git", "push", remote, refSpec)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// seedLegacyGitHooks writes every managed hook in a legacy shape, including
// Entire's marker — which is what made these survive a marker-only check.
func seedLegacyGitHooks(t *testing.T, hooksDir string, command func(hookName, args string) string) {
	t.Helper()

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	for _, hookName := range strategy.ManagedGitHookNames() {
		args, ok := legacyHookArgs[hookName]
		if !ok {
			t.Fatalf("no legacy argument list recorded for managed hook %q — add one so this test keeps covering it", hookName)
		}
		content := "#!/bin/sh\n# Entire CLI hooks\n" + command(hookName, args) + "\n"
		hookPath := filepath.Join(hooksDir, hookName)
		if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil {
			t.Fatalf("failed to seed legacy %s hook: %v", hookName, err)
		}
	}
}

// assertGitHooksInvokeBinary checks every managed hook now runs the entire binary
// and nothing inside the working tree.
func assertGitHooksInvokeBinary(t *testing.T, hooksDir string) {
	t.Helper()

	for _, hookName := range strategy.ManagedGitHookNames() {
		data, err := os.ReadFile(filepath.Join(hooksDir, hookName))
		if err != nil {
			t.Fatalf("hook %s should exist after migration: %v", hookName, err)
		}
		content := string(data)

		for _, repoContent := range []string{"scripts/entire-dev", "go run ", "git rev-parse --show-toplevel"} {
			if strings.Contains(content, repoContent) {
				t.Errorf("hook %s still reaches into the working tree (%q):\n%s", hookName, repoContent, content)
			}
		}
		if !strings.Contains(content, "entire hooks git "+hookName) {
			t.Errorf("hook %s should invoke the entire binary, got:\n%s", hookName, content)
		}
		// Every hook must be guarded, so a missing binary skips the hook rather
		// than failing the surrounding git operation.
		if !strings.Contains(content, "command -v entire") {
			t.Errorf("hook %s should be guarded by a PATH probe, got:\n%s", hookName, content)
		}
	}
}

// TestLocalDevMigration_DoctorRepairsLegacyGitHooks covers the recovery path for
// a user who pulls the release and pushes before starting a session — the one
// order in which the turn-start migration has not run yet, so the push is
// rejected and doctor is where they look.
//
// Uses --force, which is also the non-interactive path: the prompt would
// otherwise need a TTY.
func TestLocalDevMigration_DoctorRepairsLegacyGitHooks(t *testing.T) {
	t.Parallel()

	env := NewRepoWithCommit(t)
	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	seedLegacyGitHooks(t, hooksDir, func(hookName, args string) string {
		return "./scripts/entire-dev hooks git " + hookName + " " + args
	})

	if got := strategy.CheckGitHookStateInDir(context.Background(), env.RepoDir); got != strategy.GitHooksOutdated {
		t.Fatalf("seeded legacy hooks should read as outdated, got %v", got)
	}

	out, err := env.RunCLIWithError("doctor", "--force")
	if err != nil {
		t.Fatalf("doctor --force failed: %v\noutput: %s", err, out)
	}

	// It must say what it found, not silently rewrite files.
	if !strings.Contains(out, "Git hooks: OUT OF DATE") {
		t.Errorf("doctor should report the stale git hooks, got:\n%s", out)
	}
	if !strings.Contains(out, "git hooks reinstalled") {
		t.Errorf("doctor should report the repair, got:\n%s", out)
	}

	if got := strategy.CheckGitHookStateInDir(context.Background(), env.RepoDir); got != strategy.GitHooksCurrent {
		t.Errorf("git hooks should be current after doctor --force, got %v", got)
	}
	assertGitHooksInvokeBinary(t, hooksDir)

	// Second run has nothing to do and must say so rather than rewriting again.
	out, err = env.RunCLIWithError("doctor", "--force")
	if err != nil {
		t.Fatalf("second doctor --force failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "✓ Git hooks: OK") {
		t.Errorf("doctor should report healthy git hooks on the second run, got:\n%s", out)
	}
}

// TestLocalDevMigration_DoctorWarnsWithoutPromptingNonInteractively pins that a
// non-interactive `entire doctor` reports the stale hooks and exits cleanly
// instead of failing on a prompt it cannot show. Doctor is a diagnostic an agent
// or CI job runs unattended, so blocking on a confirm — or exiting non-zero
// because no TTY exists — makes it unusable there. It must also not silently
// rewrite the repo's hooks without being asked.
func TestLocalDevMigration_DoctorWarnsWithoutPromptingNonInteractively(t *testing.T) {
	t.Parallel()

	env := NewRepoWithCommit(t)
	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	seedLegacyGitHooks(t, hooksDir, func(hookName, args string) string {
		return "./scripts/entire-dev hooks git " + hookName + " " + args
	})

	// No --force, and the harness runs the CLI without a controlling terminal.
	out, err := env.RunCLIWithError("doctor")
	if err != nil {
		t.Fatalf("doctor should succeed without a TTY, got %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Git hooks: OUT OF DATE") {
		t.Errorf("doctor should still report the problem, got:\n%s", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("doctor should name the command that applies the fix, got:\n%s", out)
	}
	if strings.Contains(out, "git hooks reinstalled") {
		t.Errorf("doctor must not repair without being asked, got:\n%s", out)
	}

	// The hooks are untouched, so the problem is still there to fix later.
	if got := strategy.CheckGitHookStateInDir(context.Background(), env.RepoDir); got != strategy.GitHooksOutdated {
		t.Errorf("hooks should be left alone by a non-interactive doctor, got %v", got)
	}
}

// TestDoctor_LeavesNeverEnabledRepoAlone pins that doctor does not install hooks
// into a repository that never opted into Entire.
//
// doctor runs in any git repo, and `--force` means "don't ask me", not "take
// over this repo". Without a prior-setup guard it backed up the user's own
// pre-push hook and installed all five of Entire's — the loudest possible
// surprise from a command whose job is to report problems. Missing hooks are only
// a problem where Entire was set up; a stale hook, by contrast, is always ours to
// fix (covered by TestLocalDevMigration_DoctorRepairsLegacyGitHooks).
func TestDoctor_LeavesNeverEnabledRepoAlone(t *testing.T) {
	t.Parallel()

	// NewBareRepoEnv-style setup would still call InitEntire, so build the repo
	// without any .entire/ at all.
	env := NewRepoWithCommit(t)
	entireDir := filepath.Join(env.RepoDir, ".entire")
	if err := os.RemoveAll(entireDir); err != nil {
		t.Fatalf("failed to remove .entire: %v", err)
	}
	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	for _, hookName := range strategy.ManagedGitHookNames() {
		if err := os.Remove(filepath.Join(hooksDir, hookName)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("failed to clear hook %s: %v", hookName, err)
		}
	}

	// A hook this repo owns, which Entire must not displace.
	ownHook := "#!/bin/sh\necho my own pre-push\n"
	prePush := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(prePush, []byte(ownHook), 0o755); err != nil {
		t.Fatalf("failed to write the repo's own hook: %v", err)
	}

	out, err := env.RunCLIWithError("doctor", "--force")
	if err != nil {
		t.Fatalf("doctor --force failed: %v\noutput: %s", err, out)
	}

	if strings.Contains(out, "Git hooks: NOT INSTALLED") {
		t.Errorf("doctor should not report missing hooks for a repo that never enabled Entire, got:\n%s", out)
	}
	if strings.Contains(out, "git hooks reinstalled") {
		t.Errorf("doctor must not install hooks into a repo that never enabled Entire, got:\n%s", out)
	}

	got, readErr := os.ReadFile(prePush)
	if readErr != nil {
		t.Fatalf("the repo's own hook should still be there: %v", readErr)
	}
	if string(got) != ownHook {
		t.Errorf("the repo's own pre-push hook was replaced:\n%s", got)
	}
	if _, statErr := os.Stat(prePush + ".pre-entire"); statErr == nil {
		t.Error("doctor backed up the repo's own hook, so it installed over it")
	}
}

// TestDoctor_MigratesCommittedAbsoluteGitHookPath covers the staged rollout for
// absolute_git_hook_path.
//
// The only way to enable that feature used to write it exclusively to the
// committed .entire/settings.json, so everyone who ever used it has it there. It
// is still honored from that scope; doctor warns and offers to copy it to the
// local file, so a later release can stop honoring the committed value without
// unpinning anyone's hooks. Copy, not move: editing a tracked file would land an
// unexpected change in the user's next commit.
func TestDoctor_MigratesCommittedAbsoluteGitHookPath(t *testing.T) {
	t.Parallel()

	env := NewRepoWithCommit(t)
	settingsPath := filepath.Join(env.RepoDir, ".entire", "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{
  "enabled": true,
  "absolute_git_hook_path": true
}
`), 0o600); err != nil {
		t.Fatalf("failed to seed committed settings: %v", err)
	}

	out, err := env.RunCLIWithError("doctor", "--force")
	if err != nil {
		t.Fatalf("doctor --force failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "absolute_git_hook_path: COMMITTED") {
		t.Errorf("doctor should warn about the committed setting, got:\n%s", out)
	}
	if !strings.Contains(out, "Copied to") {
		t.Errorf("doctor --force should perform the copy, got:\n%s", out)
	}

	localPath := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("the migration should have written the local file: %v", err)
	}
	if !strings.Contains(string(local), "absolute_git_hook_path") {
		t.Errorf("local settings should carry the migrated key, got:\n%s", local)
	}

	// Copy, not move: the committed file is untouched, so the user gets no
	// surprise diff and nothing breaks if they never upgrade.
	project, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(project), "absolute_git_hook_path") {
		t.Errorf("the committed value must be left in place, got:\n%s", project)
	}

	// Having migrated, the warning stops: the committed value is now redundant.
	out, err = env.RunCLIWithError("doctor", "--force")
	if err != nil {
		t.Fatalf("second doctor --force failed: %v\noutput: %s", err, out)
	}
	if strings.Contains(out, "absolute_git_hook_path: COMMITTED") {
		t.Errorf("the warning should stop once the local file sets the key, got:\n%s", out)
	}
}
