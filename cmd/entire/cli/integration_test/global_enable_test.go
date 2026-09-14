//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// newGloballyTrackedEnv builds a repo with no repo-level Entire setup and a
// per-test user settings file the SPAWNED binary sees via extraEnv. HOME is
// isolated too: with global tracking on, every foreground `entire` command
// reconciles user-level agent hooks, which would otherwise edit the
// developer's real ~/.claude/settings.json. Returns configDir so tests can
// read the user settings file directly.
func newGloballyTrackedEnv(t *testing.T, globalEnabled bool) (env *TestEnv, extraEnv []string, configDir string) {
	t.Helper()
	env = NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# Global tracking test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/global")

	configDir = t.TempDir()
	if globalEnabled {
		if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
			[]byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
			t.Fatalf("write user-global settings: %v", err)
		}
	}
	home := t.TempDir()
	// Later duplicates win in exec.Cmd.Env, so these override the inherited
	// values. The NewHookRunner-based env.Simulate* helpers do not read
	// env.ExtraEnv — drive hooks through runClaudeHook / commitGlobalSession.
	extraEnv = []string{"ENTIRE_CONFIG_DIR=" + configDir, "HOME=" + home, "USERPROFILE=" + home}
	env.ExtraEnv = append(env.ExtraEnv, extraEnv...)
	return env, extraEnv, configDir
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
	env, extraEnv, _ := newGloballyTrackedEnv(t, true)
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
	if !env.BranchExists("entire/checkpoints/v1") {
		t.Error("checkpoint metadata ref not created by lazy enable")
	}

	// Pure path math, never an in-process classification: the per-test user
	// settings file reaches only the spawned binary via extraEnv.
	invisibleBase := globalRuntimeRoot(t, env)
	if _, err := os.Stat(filepath.Join(invisibleBase, primaryRefStampName)); err != nil {
		t.Errorf("primary-ref stamp not written by lazy enable: %v", err)
	}
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

// primaryRefStampName mirrors strategy.primaryRefStamp: the lazy setup's
// only bookkeeping file, written beside the routed runtime data.
const primaryRefStampName = "primary-ref-ready"

// globalRuntimeRoot is the git-side runtime root of env's worktree, computed
// with pure path math (repopolicy.RuntimeDir) — never via an in-process
// ClassifyRepoPolicy, which would read the test process's shared
// ENTIRE_CONFIG_DIR rather than the per-test one the spawned binary sees.
func globalRuntimeRoot(t *testing.T, env *TestEnv) string {
	t.Helper()
	repository, err := repopolicy.ResolveRepositoryAt(context.Background(), env.RepoDir)
	if err != nil {
		t.Fatalf("resolve repository: %v", err)
	}
	return repopolicy.RuntimeDir(repository)
}

// TestGlobalEnable_GitHookRouteRepairsPrimaryRef: the git-hook route runs the
// same lazy setup as the agent route. Post-commit condensation can recreate
// the v1 branch on its own, so the STAMP reappearing is the assertion that
// proves MaybeEnsureGlobalSetup ran — do not simplify it away.
func TestGlobalEnable_GitHookRouteRepairsPrimaryRef(t *testing.T) {
	t.Parallel()
	env, extraEnv, _ := newGloballyTrackedEnv(t, true)

	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      "global-test-session-3",
		"transcript_path": "",
		"prompt":          "Prime the lazy setup",
	})
	stamp := filepath.Join(globalRuntimeRoot(t, env), primaryRefStampName)
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("primary-ref stamp not written by first hook: %v", err)
	}
	if err := os.Remove(stamp); err != nil {
		t.Fatalf("remove stamp: %v", err)
	}
	deleteRef := exec.CommandContext(context.Background(), "git", "update-ref", "-d", "refs/heads/entire/checkpoints/v1")
	deleteRef.Dir = env.RepoDir
	deleteRef.Env = testutil.GitIsolatedEnv()
	if out, err := deleteRef.CombinedOutput(); err != nil {
		t.Fatalf("delete primary ref: %v\n%s", err, out)
	}

	env.WriteFile("hook-route.txt", "via git hooks\n")
	env.GitCommitWithShadowHooksAsAgent("Add file via git hook route", "hook-route.txt")

	if !env.BranchExists("entire/checkpoints/v1") {
		t.Error("git-hook route did not restore the checkpoint metadata ref")
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("git-hook route did not re-run lazy setup (stamp missing): %v", err)
	}
	assertNoWorktreeEntireDir(t, env)
}

// TestGlobalEnable_AgentRouteReinstallsGitHooks: hook presence is re-checked
// on every hook, so deleted git hooks come back on the next agent activity
// without any marker bookkeeping.
func TestGlobalEnable_AgentRouteReinstallsGitHooks(t *testing.T) {
	t.Parallel()
	env, extraEnv, _ := newGloballyTrackedEnv(t, true)
	ctx := context.Background()
	prompt := map[string]string{
		"session_id":      "global-test-session-4",
		"transcript_path": "",
		"prompt":          "Prime the lazy setup",
	}

	runClaudeHook(t, env, extraEnv, "user-prompt-submit", prompt)
	if !strategy.IsGitHookInstalledInDir(ctx, env.RepoDir) {
		t.Fatal("git hooks not installed after first hook event")
	}
	if err := os.RemoveAll(filepath.Join(env.RepoDir, ".git", "hooks")); err != nil {
		t.Fatalf("remove hooks dir: %v", err)
	}

	runClaudeHook(t, env, extraEnv, "user-prompt-submit", prompt)
	if !strategy.IsGitHookInstalledInDir(ctx, env.RepoDir) {
		t.Error("agent route did not reinstall the deleted git hooks")
	}
	assertNoWorktreeEntireDir(t, env)
}

func TestGlobalEnable_TierAbsent_CreatesNothing(t *testing.T) {
	t.Parallel()
	env, extraEnv, _ := newGloballyTrackedEnv(t, false)

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

// statusGlobalJSON is the global_tracking slice of `entire status --json`.
type statusGlobalJSON struct {
	GlobalTracking struct {
		ActivationSource string `json:"activation_source"`
		TrustState       string `json:"trust_state"`
		TrustSource      string `json:"trust_source"`
	} `json:"global_tracking"`
}

// statusGlobalJSONOutput runs `status --json` through the real binary,
// keeping stderr (where the post-run's hook notices land) out of the JSON.
func statusGlobalJSONOutput(t *testing.T, env *TestEnv) statusGlobalJSON {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), getTestBinary(), "status", "--json")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status --json failed: %v\nStderr: %s", err, stderr.String())
	}
	var parsed statusGlobalJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse status --json: %v\nOutput: %s", err, out)
	}
	return parsed
}

// TestGlobalEnable_ExplicitEnableAfterGlobalCapture: a repo first captured
// globally, then explicitly enabled, switches to repo-level activation and
// the worktree layout; enable records trust (path identity here — no origin);
// the old git-side runtime data is left in place (no migration).
func TestGlobalEnable_ExplicitEnableAfterGlobalCapture(t *testing.T) {
	t.Parallel()
	env, extraEnv, configDir := newGloballyTrackedEnv(t, true)

	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      "global-then-local-1",
		"transcript_path": "",
		"prompt":          "Captured globally",
	})
	oldRoot := globalRuntimeRoot(t, env)
	if _, err := os.Stat(filepath.Join(oldRoot, "metadata", "global-then-local-1", "prompt.txt")); err != nil {
		t.Fatalf("global capture did not land under the git common dir: %v", err)
	}
	before := statusGlobalJSONOutput(t, env)
	if before.GlobalTracking.ActivationSource != "global" || before.GlobalTracking.TrustState != "untrusted" {
		t.Fatalf("pre-enable status = %+v, want global/untrusted", before.GlobalTracking)
	}

	out := env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	if !strings.Contains(out, "Trusted") {
		t.Fatalf("enable did not record trust while global tracking is on:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(env.RepoDir, ".entire", "settings.json")); err != nil {
		t.Fatalf("explicit enable did not write repo settings: %v", err)
	}
	after := statusGlobalJSONOutput(t, env)
	if after.GlobalTracking.ActivationSource != "local" || after.GlobalTracking.TrustState != "trusted" || after.GlobalTracking.TrustSource != "repo" {
		t.Fatalf("post-enable status = %+v, want local/trusted/repo", after.GlobalTracking)
	}
	data, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var us repopolicy.UserSettings
	if err := json.Unmarshal(data, &us); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(us.Global.TrustedPaths, func(p string) bool {
		resolved, rerr := filepath.EvalSymlinks(p)
		return rerr == nil && resolved == env.RepoDir
	}) {
		t.Fatalf("trusted_paths = %v, want the repo root", us.Global.TrustedPaths)
	}

	// A new session now lands in the worktree layout; the old data is left.
	runClaudeHook(t, env, extraEnv, "user-prompt-submit", map[string]string{
		"session_id":      "global-then-local-2",
		"transcript_path": "",
		"prompt":          "Captured locally",
	})
	if _, err := os.Stat(filepath.Join(env.RepoDir, ".entire", "metadata", "global-then-local-2", "prompt.txt")); err != nil {
		t.Errorf("post-enable capture did not land in the worktree layout: %v", err)
	}
	if _, err := os.Stat(oldRoot); err != nil {
		t.Errorf("git-side runtime data from the global phase should be left in place: %v", err)
	}
}

// TestGlobalEnable_CommittedSettingsInFreshCloneAreGated: a fresh clone that
// brings a committed .entire/settings.json {"enabled":true} is repo-enabled
// exactly as on main — but while global tracking is on, its checkpoints are
// held until trusted, on both storage backends.
func TestGlobalEnable_CommittedSettingsInFreshCloneAreGated(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		src, extraEnv, _ := newGloballyTrackedEnv(t, true)
		src.CheckpointStore = backend
		src.WriteFile(".entire/settings.json", `{"enabled":true}`)
		src.GitAdd(".entire/settings.json")
		src.GitCommit("Commit Entire settings")
		bare := src.SetupEmptyNamedBareRemote("origin")
		push := exec.CommandContext(t.Context(), "git", "push", "origin", "HEAD")
		push.Dir = src.RepoDir
		push.Env = testutil.GitIsolatedEnv()
		if out, err := push.CombinedOutput(); err != nil {
			t.Fatalf("seed origin: %v\n%s", err, out)
		}

		clone := src.CloneFromWithoutInit(bare)
		routeHermeticOrigin(t, clone, bare)
		if _, err := os.Stat(filepath.Join(clone.RepoDir, ".entire", "settings.json")); err != nil {
			t.Fatalf("clone did not bring the committed settings: %v", err)
		}
		status := statusGlobalJSONOutput(t, clone)
		if status.GlobalTracking.ActivationSource != "local" || status.GlobalTracking.TrustState != "untrusted" {
			t.Fatalf("fresh clone status = %+v, want local/untrusted", status.GlobalTracking)
		}

		commitGlobalSession(t, clone, extraEnv, "clone-session-1", "clone.txt", "held\n", "Create clone file")
		heldID := clone.GetCheckpointIDFromCommitMessage(clone.GetHeadHash())
		if heldID == "" {
			t.Fatal("commit has no Entire-Checkpoint trailer; nothing to hold")
		}
		output := gitPushHeadWithHooksOutput(t, clone)
		if got := strings.Count(output, heldSyncMessageFragment); got != 1 {
			t.Errorf("held push should print exactly one hold explanation, got %d in:\n%s", got, output)
		}
		if !clone.BranchExistsOnRemote(bare, clone.GetCurrentBranch()) {
			t.Error("the user's branch must land on the remote despite the hold")
		}
		if anyRefUnderPrefix(t, bare) {
			t.Error("no refs/entire/* may reach the remote while the repo is untrusted")
		}
		if clone.CheckpointExistsOnRemote(bare, heldID) {
			t.Error("the held checkpoint must not reach the remote while untrusted")
		}

		if out := clone.RunCLI("trust"); !strings.Contains(out, "Trusted") {
			t.Fatalf("entire trust did not confirm trust:\n%s", out)
		}
		clone.WriteFile("after-trust.txt", "synced\n")
		clone.GitAdd("after-trust.txt")
		clone.GitCommit("Add after-trust file")
		output = gitPushHeadWithHooksOutput(t, clone)
		if strings.Contains(output, heldSyncMessageFragment) {
			t.Errorf("a trusted push is silent about holds, got:\n%s", output)
		}
		if !clone.CheckpointExistsOnRemote(bare, heldID) {
			t.Errorf("held checkpoint %s should drain on the first trusted push", heldID)
		}
	})
}
