//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func newGloballyTrackedEnv(t *testing.T, globalEnabled bool) (*TestEnv, []string) {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# Global tracking test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/global")

	configDir := t.TempDir()
	if globalEnabled {
		if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
			[]byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
			t.Fatalf("write user-global settings: %v", err)
		}
	}
	extraEnv := []string{"ENTIRE_CONFIG_DIR=" + configDir}
	env.ExtraEnv = append(env.ExtraEnv, extraEnv...)
	return env, extraEnv
}

func gitStatusPorcelain(t *testing.T, env *TestEnv) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "status", "--porcelain")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status --porcelain: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func runClaudeHook(t *testing.T, env *TestEnv, extraEnv []string, hookName string, input map[string]string) {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal hook input: %v", err)
	}
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	if err := runner.runHookInRepoDirWithExtraEnv(hookName, inputJSON, extraEnv); err != nil {
		t.Fatalf("hook %s: %v", hookName, err)
	}
}

func assertNoWorktreeEntireDir(t *testing.T, env *TestEnv) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(env.RepoDir, ".entire")); !os.IsNotExist(err) {
		t.Errorf(".entire exists in the worktree of a globally tracked repo (err=%v)", err)
	}
}

func TestGlobalEnable_LazyInvisibleSetup(t *testing.T) {
	t.Parallel()
	env, extraEnv := newGloballyTrackedEnv(t, true)
	ctx := context.Background()

	sessionID := "global-test-session-1"
	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
		"prompt":          "Create hello file",
	})

	if !strategy.IsGitHookInstalledInDir(ctx, env.RepoDir) {
		t.Error("git hooks not installed after first hook event")
	}
	repository, err := repopolicy.ResolveRepositoryAt(ctx, env.RepoDir)
	if err != nil {
		t.Fatalf("resolve repository: %v", err)
	}
	record, found, err := repopolicy.ReadSetupRecord(repository)
	if err != nil || !found {
		t.Fatalf("setup record = (%+v, %v, %v), want per-worktree record", record, found, err)
	}
	if record.GitHooksSpec != 1 || record.PrimaryRefSpec != 1 {
		t.Errorf("setup components not recorded: %+v", record)
	}
	if !env.BranchExists("entire/checkpoints/v1") {
		t.Error("checkpoint metadata ref not created by lazy enable")
	}

	invisibleBase := filepath.Join(env.RepoDir, ".git", "entire", "worktree", paths.HashWorktreeID(""))
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "" {
		t.Errorf("git status not empty after hook event:\n%s", status)
	}
	promptPath := filepath.Join(invisibleBase, "metadata", sessionID, "prompt.txt")
	if _, err := os.Stat(promptPath); err != nil {
		t.Errorf("prompt.txt not routed to git common dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(invisibleBase, "logs", "entire.log")); err != nil {
		t.Errorf("hook log not routed to git common dir: %v", err)
	}

	env.WriteFile("hello.txt", "hello world\n")
	builder := NewTranscriptBuilder()
	builder.AddUserMessage("Create hello file")
	toolID := builder.AddToolUse("mcp__acp__Write", "hello.txt", "hello world\n")
	builder.AddToolResult(toolID)
	builder.AddAssistantMessage("Done!")
	transcriptPath := filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")
	if err := builder.WriteToFile(transcriptPath); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	runClaudeHook(t, env, extraEnv, "stop", map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	})

	if _, err := os.Stat(filepath.Join(invisibleBase, "metadata", sessionID, "full.jsonl")); err != nil {
		t.Errorf("full.jsonl not routed to git common dir: %v", err)
	}
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "?? hello.txt" {
		t.Errorf("git status should show only the agent's edit, got:\n%s", status)
	}

	env.GitCommitWithShadowHooksAsAgent("Add hello file", "hello.txt")
	commitMsg := env.GetCommitMessage(env.GetHeadHash())
	checkpointID, found := trailers.ParseCheckpoint(commitMsg)
	if !found {
		t.Errorf("commit has no Entire-Checkpoint trailer:\n%s", commitMsg)
	}
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "" {
		t.Errorf("git status not empty after commit:\n%s", status)
	}

	statusOut, statusErr := env.RunCLIWithError("status")
	if statusErr != nil {
		t.Errorf("entire status failed in a globally tracked repo: %v\n%s", statusErr, statusOut)
	}
	listOut := env.RunCLI("checkpoint", "list", "--json")
	if !strings.Contains(listOut, string(checkpointID)) {
		t.Errorf("checkpoint list --json does not show the condensed checkpoint %s:\n%s", checkpointID, listOut)
	}
	if !strings.Contains(listOut, sessionID) {
		t.Errorf("checkpoint list --json does not attribute the checkpoint to session %s:\n%s", sessionID, listOut)
	}
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "" {
		t.Errorf("git status not empty after CLI read-back:\n%s", status)
	}
}

func TestGlobalEnable_GitHookRouteTriggersSetup(t *testing.T) {
	t.Parallel()
	env, extraEnv := newGloballyTrackedEnv(t, true)
	ctx := context.Background()

	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      "global-test-session-3",
		"transcript_path": "",
		"prompt":          "Prime the lazy setup",
	})
	if !strategy.IsGitHookInstalledInDir(ctx, env.RepoDir) {
		t.Fatal("git hooks not installed after first hook event")
	}

	repository, err := repopolicy.ResolveRepositoryAt(ctx, env.RepoDir)
	if err != nil {
		t.Fatalf("resolve repository: %v", err)
	}
	setupPath := filepath.Join(repopolicy.WorktreeRegistryDir(repository.GitCommonDir, repository.WorktreeKey), "setup.json")
	if err := os.Remove(setupPath); err != nil {
		t.Fatalf("remove setup record: %v", err)
	}

	env.WriteFile("hook-route.txt", "via git hooks\n")
	env.GitCommitWithShadowHooksAsAgent("Add file via git hook route", "hook-route.txt")

	record, found, err := repopolicy.ReadSetupRecord(repository)
	if err != nil || !found || record.GitHooksSpec != 1 || record.PrimaryRefSpec != 1 {
		t.Errorf("git-hook route did not restore the setup record: (%+v, %v, %v)", record, found, err)
	}
	assertNoWorktreeEntireDir(t, env)
}

func TestGlobalEnable_TierAbsent_CreatesNothing(t *testing.T) {
	t.Parallel()
	env, extraEnv := newGloballyTrackedEnv(t, false)

	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      "global-test-session-2",
		"transcript_path": "",
		"prompt":          "Do something",
	})

	if strategy.IsGitHookInstalledInDir(context.Background(), env.RepoDir) {
		t.Error("git hooks installed although the global tier is absent")
	}
	if _, err := os.Lstat(filepath.Join(env.RepoDir, ".git", "entire")); !os.IsNotExist(err) {
		t.Errorf(".git/entire created although the global tier is absent (err=%v)", err)
	}
	if env.BranchExists("entire/checkpoints/v1") {
		t.Error("checkpoint metadata ref created although the global tier is absent")
	}
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "" {
		t.Errorf("git status not empty:\n%s", status)
	}
}
