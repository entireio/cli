//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/require"
)

const validLefthookConfig = "pre-commit: {}\n"

func TestLefthookAlreadyActiveThenEntireEnablePreservesEveryHook(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	writeLefthookConfig(t, env.RepoDir)
	installSimulatedLefthookHooks(t, env.RepoDir)

	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	assertEveryLefthookDispatch(t, env, env.RepoDir)
}

func TestEntireNativeHooksThenLefthookRefreshPreservesEveryHook(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	for _, hook := range strategy.ManagedGitHookNames() {
		data, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "hooks", hook))
		require.NoError(t, err)
		require.Contains(t, string(data), "Entire CLI hooks")
	}
	// Exercise the native wrappers before any manager config exists. The helper
	// allocates a fresh fake-Entire log on each call, so these counts cannot be
	// satisfied by the post-refresh dispatch below.
	assertEveryLefthookDispatch(t, env, env.RepoDir)

	// Lefthook is introduced after Entire. The next setup pass publishes the
	// durable local integration before Lefthook refreshes its shared wrappers.
	writeLefthookConfig(t, env.RepoDir)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	installSimulatedLefthookHooks(t, env.RepoDir)

	assertEveryLefthookDispatch(t, env, env.RepoDir)
}

func TestLefthookIntegrationIsWorktreeLocalWithSharedHooks(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	writeLefthookConfig(t, env.RepoDir)
	installSimulatedLefthookHooks(t, env.RepoDir)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")

	linked := filepath.Join(t.TempDir(), "linked")
	cmd := exec.CommandContext(t.Context(), "git", "worktree", "add", "-b", "feature/linked-lefthook", linked)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	writeLefthookConfig(t, linked)
	runCLIAt(t, env, linked, "enable", "--agent", agentClaudeCode, "--telemetry=false")

	for _, root := range []string{env.RepoDir, linked} {
		data, readErr := os.ReadFile(filepath.Join(root, "lefthook-local.yml"))
		require.NoError(t, readErr, root)
		require.Contains(t, string(data), "entire-cli-owned:lefthook:v1", root)
		for _, hook := range strategy.ManagedGitHookNames() {
			_, statErr := os.Stat(filepath.Join(root, ".lefthook-local", hook, "entire.sh"))
			require.NoError(t, statErr, "%s in %s", hook, root)
		}
	}

	mainHook := gitPath(t, env, env.RepoDir, "hooks/pre-push")
	linkedHook := gitPath(t, env, linked, "hooks/pre-push")
	require.Equal(t, mainHook, linkedHook, "linked worktree must retain the effective shared hook path")
	assertEveryLefthookDispatch(t, env, linked)
}

func TestUninstallLefthookIntegrationRollsBackObstructionAndRetries(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	writeLefthookConfig(t, env.RepoDir)
	installSimulatedLefthookHooks(t, env.RepoDir)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")

	obstruction := filepath.Join(env.RepoDir, ".lefthook-local", "pre-push", "entire.sh")
	require.NoError(t, os.Remove(obstruction))
	require.NoError(t, os.Mkdir(obstruction, 0o755))

	out, err := env.RunCLIWithError("disable", "--uninstall", "--force")
	require.Error(t, err, out)
	require.Contains(t, out, "inspect owned Lefthook script pre-push")
	data, readErr := os.ReadFile(filepath.Join(env.RepoDir, "lefthook-local.yml"))
	require.NoError(t, readErr)
	require.Contains(t, string(data), "entire-cli-owned:lefthook:v1", "failed removal must restore earlier mutations")
	_, statErr := os.Stat(filepath.Join(env.RepoDir, ".lefthook-local", "prepare-commit-msg", "entire.sh"))
	require.NoError(t, statErr, "failed removal must restore already-removed scripts")

	require.NoError(t, os.Remove(obstruction))
	env.RunCLI("disable", "--uninstall", "--force")
	assertNoOwnedLefthookArtifacts(t, env.RepoDir)
	for _, hook := range strategy.ManagedGitHookNames() {
		data, hookErr := os.ReadFile(filepath.Join(env.RepoDir, ".git", "hooks", hook))
		require.NoError(t, hookErr)
		require.Contains(t, string(data), "lefthook", "uninstall must retain manager-owned wrapper %s", hook)
	}
}

func writeLefthookConfig(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, "lefthook.yml"), []byte(validLefthookConfig), 0o644))
}

func installSimulatedLefthookHooks(t *testing.T, repoRoot string) {
	t.Helper()
	hooksDir := filepath.Join(repoRoot, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	for _, hook := range strategy.ManagedGitHookNames() {
		content := fmt.Sprintf(`#!/bin/sh

if [ "$LEFTHOOK_VERBOSE" = "1" -o "$LEFTHOOK_VERBOSE" = "true" ]; then
  set -x
fi

if [ "$LEFTHOOK" = "0" ]; then
  exit 0
fi

# lefthook generated wrapper
call_lefthook()
{
  shift
  hook="$1"
  shift
  exec "$PWD/.lefthook-local/$hook/entire.sh" "$@"
}

call_lefthook run "%s" "$@"
`, hook)
		require.NoError(t, os.WriteFile(filepath.Join(hooksDir, hook), []byte(content), 0o755))
	}
}

func assertEveryLefthookDispatch(t *testing.T, env *TestEnv, worktree string) {
	t.Helper()
	fakeBin := t.TempDir()
	logBase := filepath.Join(t.TempDir(), "hook")
	fake := `#!/bin/sh
hook="$3"
printf '%s\n' "$@" > "$ENTIRE_HOOK_LOG.$hook.args"
cat > "$ENTIRE_HOOK_LOG.$hook.stdin"
printf 'call\n' >> "$ENTIRE_HOOK_LOG.$hook.calls"
if [ "$hook" = pre-push ]; then exit "${ENTIRE_PRE_PUSH_EXIT:-0}"; fi
`
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "entire"), []byte(fake), 0o755))

	msg := filepath.Join(worktree, ".git", "COMMIT_EDITMSG")
	cases := []struct {
		name      string
		args      []string
		stdin     string
		wantArgs  []string
		wantStdin string
		wantExit  bool
	}{
		{name: "prepare-commit-msg", args: []string{msg, "message"}, wantArgs: []string{"hooks", "git", "prepare-commit-msg", msg, "message"}},
		{name: "commit-msg", args: []string{msg}, wantArgs: []string{"hooks", "git", "commit-msg", msg}},
		{name: "post-commit", wantArgs: []string{"hooks", "git", "post-commit"}},
		{name: "post-rewrite", args: []string{"amend"}, stdin: "old new\n", wantArgs: []string{"hooks", "git", "post-rewrite", "amend"}, wantStdin: "old new\n"},
		{name: "pre-push", args: []string{"origin", "https://example.test/repo.git"}, wantArgs: []string{"hooks", "git", "pre-push", "origin", "https://example.test/repo.git"}, wantExit: true},
	}
	for _, tc := range cases {
		script := gitPath(t, env, worktree, "hooks/"+tc.name)
		cmd := execx.NonInteractive(context.Background(), script, tc.args...)
		cmd.Dir = worktree
		cmd.Env = append(env.cliEnv(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"), "ENTIRE_HOOK_LOG="+logBase, "ENTIRE_PRE_PUSH_EXIT=23")
		cmd.Stdin = strings.NewReader(tc.stdin)
		out, err := cmd.CombinedOutput()
		if tc.wantExit {
			require.Error(t, err, "%s must propagate the Entire pre-push block; output=%s", tc.name, out)
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			require.Equal(t, 23, exitErr.ExitCode())
		} else {
			require.NoError(t, err, "%s output=%s", tc.name, out)
		}
		require.Equal(t, tc.wantArgs, readLines(t, logBase+"."+tc.name+".args"), tc.name)
		require.Equal(t, []string{"call"}, readLines(t, logBase+"."+tc.name+".calls"), "%s must run fake Entire exactly once", tc.name)
		stdin, readErr := os.ReadFile(logBase + "." + tc.name + ".stdin")
		require.NoError(t, readErr)
		require.Equal(t, tc.wantStdin, string(stdin), tc.name)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func gitPath(t *testing.T, env *TestEnv, dir, path string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--path-format=absolute", "--git-path", path)
	cmd.Dir = dir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func runCLIAt(t *testing.T, env *TestEnv, dir string, args ...string) {
	t.Helper()
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Dir = dir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func assertNoOwnedLefthookArtifacts(t *testing.T, repoRoot string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(repoRoot, "lefthook-local.yml")); err == nil {
		require.NotContains(t, string(data), "entire-cli-owned:lefthook:v1")
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	for _, hook := range strategy.ManagedGitHookNames() {
		_, err := os.Stat(filepath.Join(repoRoot, ".lefthook-local", hook, "entire.sh"))
		require.ErrorIs(t, err, os.ErrNotExist, hook)
	}
}
