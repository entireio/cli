//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The pre-push hook `entire enable` writes reads git's ref list, gives it to
// Entire, and replays it to the user's own hook chained after it. This runs that
// generated hook under a real `git push` with the real binary, not a stand-in
// script, so the user's hook must still receive git's ref list.
func TestPrePushHook_ReplaysGitRefListToChainedHook(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()

	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	recorded := filepath.Join(t.TempDir(), "user-hook.stdin")
	userHook := "#!/bin/sh\ncat > " + recorded + "\n"
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("create hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(userHook), 0o755); err != nil {
		t.Fatalf("write user pre-push hook: %v", err)
	}

	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false", "--absolute-git-hook-path")
	installed, err := os.ReadFile(filepath.Join(hooksDir, "pre-push"))
	if err != nil {
		t.Fatalf("read installed pre-push hook: %v", err)
	}
	if !strings.Contains(string(installed), strategy.PrePushStdinRefsEnv+"=1") {
		t.Fatalf("enable did not install the ref-list pre-push line:\n%s", installed)
	}

	env.WriteFile("work.txt", "work")
	env.GitAdd("work.txt")
	env.GitCommit("Add work")
	head := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD"))
	branch := env.GetCurrentBranch()

	cmd := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	if out, pushErr := cmd.CombinedOutput(); pushErr != nil {
		t.Fatalf("git push failed: %v\n%s", pushErr, out)
	}

	got, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatalf("the chained user hook did not run: %v", err)
	}
	// Pushing HEAD, git names the local side "HEAD".
	want := "HEAD " + head + " refs/heads/" + branch + " "
	if !strings.HasPrefix(string(got), want) {
		t.Fatalf("the chained user hook got %q, want git's ref list starting with %q", got, want)
	}
}
