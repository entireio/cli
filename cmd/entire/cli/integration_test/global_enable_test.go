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
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// newGloballyTrackedEnv creates a repo with a commit on a feature branch and
// NO repo-level Entire setup (no InitEntire, no .entire anywhere), plus a
// per-test ENTIRE_CONFIG_DIR. When globalEnabled is true, that config dir
// contains a user-global settings file enabling the global tier.
//
// The config dir override rides on env.ExtraEnv (cliEnv/gitHookEnv append it
// after the TestMain-wide ENTIRE_CONFIG_DIR; last duplicate wins in exec.Cmd)
// and is returned for hook invocations that need it passed explicitly.
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

// gitStatusPorcelain runs `git status --porcelain` in the repo and returns
// the trimmed output. Empty output is the invisible-mode product guarantee:
// global tracking must never surface anything in the user's git status.
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

// TestGlobalEnable_LazyInvisibleSetup exercises the full lazy invisible
// enable in a repo with no repo-level setup and the user-global tier on:
// the first hook event installs git hooks and marks the clone, every runtime
// write lands under .git/entire/worktree/, and git status stays clean.
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

	// Lazy enable happened, entirely inside .git/.
	if !strategy.IsGitHookInstalledInDir(ctx, env.RepoDir) {
		t.Error("git hooks not installed after first hook event")
	}
	prefsData, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "entire", "preferences.json"))
	if err != nil {
		t.Fatalf("clone preferences not written: %v", err)
	}
	var prefs struct {
		GlobalSetupCompleted bool `json:"global_setup_completed"`
	}
	if err := json.Unmarshal(prefsData, &prefs); err != nil {
		t.Fatalf("parse clone preferences: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Errorf("clone preferences not marked global_setup_completed: %s", prefsData)
	}
	if !env.BranchExists("entire/checkpoints/v1") {
		t.Error("checkpoint metadata ref not created by lazy enable")
	}

	// The invisible guarantee: no worktree files, empty git status. The
	// routed base is namespaced per worktree; env.RepoDir is a main
	// worktree, whose key hashes the empty worktree ID.
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

	// Full turn: the agent edits a file, then Stop saves a checkpoint.
	// The transcript lives in the agent's project dir, outside the repo.
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

	// Checkpoint metadata routed; only the agent's edit shows in status.
	if _, err := os.Stat(filepath.Join(invisibleBase, "metadata", sessionID, "full.jsonl")); err != nil {
		t.Errorf("full.jsonl not routed to git common dir: %v", err)
	}
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "?? hello.txt" {
		t.Errorf("git status should show only the agent's edit, got:\n%s", status)
	}

	// A user commit through the installed hooks links the checkpoint.
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

	// Read-back through the CLI: routed data must be visible to the commands
	// users actually run, not just present on disk.
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
	// The read commands themselves must not break invisibility either.
	assertNoWorktreeEntireDir(t, env)
	if status := gitStatusPorcelain(t, env); status != "" {
		t.Errorf("git status not empty after CLI read-back:\n%s", status)
	}
}

// TestGlobalEnable_GitHookRouteTriggersSetup pins the git-hook half of the
// lazy-setup trigger: with hooks already present but the clone-prefs marker
// absent (e.g. cleared by doctor after drift), a plain git-hook invocation —
// no agent hook involved — re-runs the setup and restores the marker.
func TestGlobalEnable_GitHookRouteTriggersSetup(t *testing.T) {
	t.Parallel()
	env, extraEnv := newGloballyTrackedEnv(t, true)
	ctx := context.Background()

	// First agent-hook activity performs the initial setup.
	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      "global-test-session-3",
		"transcript_path": "",
		"prompt":          "Prime the lazy setup",
	})
	if !strategy.IsGitHookInstalledInDir(ctx, env.RepoDir) {
		t.Fatal("git hooks not installed after first hook event")
	}

	// Clear the marker, keep the hooks — the doctor-after-drift shape.
	prefsPath := filepath.Join(env.RepoDir, ".git", "entire", "preferences.json")
	if err := os.Remove(prefsPath); err != nil {
		t.Fatalf("remove clone preferences: %v", err)
	}

	// A plain git commit through the installed hooks (prepare-commit-msg +
	// post-commit route) must re-run the setup and restore the marker.
	env.WriteFile("hook-route.txt", "via git hooks\n")
	env.GitCommitWithShadowHooksAsAgent("Add file via git hook route", "hook-route.txt")

	prefsData, err := os.ReadFile(prefsPath)
	if err != nil {
		t.Fatalf("clone preferences not rewritten by the git-hook route: %v", err)
	}
	var prefs struct {
		GlobalSetupCompleted bool `json:"global_setup_completed"`
	}
	if err := json.Unmarshal(prefsData, &prefs); err != nil {
		t.Fatalf("parse clone preferences: %v", err)
	}
	if !prefs.GlobalSetupCompleted {
		t.Errorf("git-hook route did not restore the setup marker: %s", prefsData)
	}
	assertNoWorktreeEntireDir(t, env)
}

// TestGlobalEnable_TierAbsent_CreatesNothing is the negative: with no
// user-global settings, a hook event in a repo without repo-level setup must
// leave zero traces — no hooks, no clone preferences, no runtime data,
// nothing in the worktree.
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
