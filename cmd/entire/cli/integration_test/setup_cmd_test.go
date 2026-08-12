//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// RunEnableWithAccessibleMode runs `entire enable` without --strategy flag in accessible mode.
// It provides stdin input to answer the telemetry prompt.
func (env *TestEnv) RunEnableWithAccessibleMode() string {
	env.T.Helper()

	// Run CLI with ACCESSIBLE=1 for non-interactive prompts
	// Provide "no" for telemetry
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "enable")
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(), "ACCESSIBLE=1")
	// Provide input for telemetry prompt
	cmd.Stdin = strings.NewReader("no\n")

	output, err := cmd.CombinedOutput()
	if err != nil {
		env.T.Fatalf("enable command failed: %v\nOutput: %s", err, output)
	}
	return string(output)
}

// SetEnabled updates the enabled state in .entire/settings file
func (env *TestEnv) SetEnabled(enabled bool) {
	env.T.Helper()

	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)

	// Read existing settings
	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			env.T.Fatalf("failed to parse settings: %v", err)
		}
	} else {
		settings = make(map[string]interface{})
	}

	// Update enabled state
	settings["enabled"] = enabled

	// Write back
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		env.T.Fatalf("failed to marshal settings: %v", err)
	}
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		env.T.Fatalf("failed to write settings: %v", err)
	}
}

func TestEnableDisable(t *testing.T) {
	t.Parallel()
	env := NewRepoEnv(t)
	// Initially should be enabled (default)
	stdout := env.RunCLI("status")
	if !strings.Contains(stdout, "Enabled") {
		t.Errorf("Expected status to show 'Enabled', got: %s", stdout)
	}

	// Disable (using --project so re-enable can override cleanly)
	stdout = env.RunCLI("disable", "--project")
	if !strings.Contains(stdout, "disabled") {
		t.Errorf("Expected disable output to contain 'disabled', got: %s", stdout)
	}

	// Check status is now disabled
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "Disabled") {
		t.Errorf("Expected status to show 'Disabled', got: %s", stdout)
	}

	// Re-enable (using --agent for non-interactive mode)
	stdout = env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	if !strings.Contains(stdout, "Ready.") {
		t.Errorf("Expected enable output to contain 'Ready.', got: %s", stdout)
	}

	// Check status is now enabled
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "Enabled") {
		t.Errorf("Expected status to show 'Enabled', got: %s", stdout)
	}
}

func TestCheckpointListPendingBlockedWhenDisabled(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	// Disable Entire
	env.SetEnabled(false)

	// Try to run checkpoint list --pending --json - should show disabled message (not error)
	stdout, err := env.RunCLIWithError("checkpoint", "list", "--pending", "--json")
	if err != nil {
		t.Fatalf("checkpoint list --pending --json command failed unexpectedly: %v\nOutput: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Entire is disabled") {
		t.Errorf("Expected disabled message, got: %s", stdout)
	}
	if !strings.Contains(stdout, "entire enable") {
		t.Errorf("Expected message to mention 'entire enable', got: %s", stdout)
	}
}

func TestHooksSilentWhenDisabled(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	// Create an untracked file
	env.WriteFile("newfile.txt", "content")

	// Disable Entire
	env.SetEnabled(false)

	// Run hook - should exit silently (no error, no state file created)
	err := env.SimulateUserPromptSubmit("test-session-disabled")
	if err != nil {
		t.Fatalf("Hook should exit silently when disabled, got error: %v", err)
	}

	// Verify no state file was created (hook exited early)
	statePath := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-prompt-test-session-disabled.json")
	if _, err := os.Stat(statePath); err == nil {
		t.Error("pre-prompt state file should NOT exist when disabled")
	}
}

func TestStatusWhenDisabled(t *testing.T) {
	t.Parallel()
	env := NewRepoEnv(t)
	// Disable Entire
	env.SetEnabled(false)

	// Status command should still work and show disabled
	stdout := env.RunCLI("status")
	if !strings.Contains(stdout, "Disabled") {
		t.Errorf("Expected status to show 'Disabled', got: %s", stdout)
	}
}

func TestEnableWhenDisabled(t *testing.T) {
	t.Parallel()
	env := NewRepoEnv(t)
	// Disable Entire
	env.SetEnabled(false)

	// Enable command should work (using --agent for non-interactive mode)
	stdout := env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	if !strings.Contains(stdout, "Ready.") {
		t.Errorf("Expected enable output to contain 'Ready.', got: %s", stdout)
	}

	// Verify it's now enabled
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "Enabled") {
		t.Errorf("Expected status to show 'Enabled' after re-enabling, got: %s", stdout)
	}
}

func TestEnableDefaultStrategy(t *testing.T) {
	t.Parallel()

	// Create a basic test environment with just a git repo (no Entire init)
	env := NewTestEnv(t)
	env.InitRepo()

	// Run entire enable without --strategy flag
	// This tests that the default strategy is manual-commit
	// We use stdin to answer the telemetry prompt
	stdout := env.RunEnableWithAccessibleMode()

	// Verify output shows "Ready." (simplified flow doesn't mention strategy)
	if !strings.Contains(stdout, "Ready.") {
		t.Errorf("Expected output to contain 'Ready.', got: %s", stdout)
	}

	// Verify settings file exists and has enabled field
	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("Failed to read settings file: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Failed to parse settings: %v", err)
	}

	enabled, ok := settings["enabled"].(bool)
	if !ok {
		t.Fatalf("Enabled not found in settings: %v", settings)
	}

	if !enabled {
		t.Error("Expected enabled to be true")
	}

	// Verify status shows the enabled state
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "Enabled") {
		t.Errorf("Expected status to show 'Enabled', got: %s", stdout)
	}
}

// TestHooksRunAfterLocalOnlyEnable is a full-flow reproduction of the
// `entire enable --local` regression: only .entire/settings.local.json
// exists, and the hooks (gated on settings.IsSetUpAndEnabled) silently
// no-op'd because that check only looked at settings.json — so a commit
// produced no checkpoint.
//
// This drives the real hook binary end-to-end: a session, a
// user-prompt-submit, a file change, a stop, and a commit — then asserts the
// commit actually carries an Entire-Checkpoint trailer (i.e. the hooks ran
// and a checkpoint was saved). Complements TestHooksSilentWhenDisabled above,
// which covers the opposite case.
func TestHooksRunAfterLocalOnlyEnable(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/local-only")

	// Simulate `entire enable --local`: only settings.local.json exists.
	entireDir := filepath.Join(env.RepoDir, ".entire")
	if err := os.MkdirAll(filepath.Join(entireDir, "tmp"), 0o755); err != nil {
		t.Fatalf("mkdir .entire/tmp: %v", err)
	}
	localSettings := `{"enabled":true,"local_dev":true,"strategy_options":{"filtered_fetches":true}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(entireDir, "settings.json")); err == nil {
		t.Fatal("precondition: settings.json must not exist for the enable --local scenario")
	}

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create a hello file"); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile("hello.txt", "hello")
	session.CreateTranscript("Create a hello file", []FileChange{{Path: "hello.txt", Content: "hello"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}
	env.GitCommitWithShadowHooksAsAgent("add hello", "hello.txt")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if cpID == "" {
		t.Fatal("commit has no Entire-Checkpoint trailer — hooks silently no-op'd with only settings.local.json")
	}
}

// TestEnableReenablesProjectScopeAfterProjectDisable is a full-flow
// reproduction of a re-enable regression: after `entire disable --project`,
// running `entire enable --checkpoint-remote ...` with no --project/--local
// reported success but wrote the enabled flag to .entire/settings.local.json,
// leaving the project .entire/settings.json the user disabled still
// enabled=false.
//
// Because settings.local.json (enabled:true) overrides settings.json in the
// merged view that both `entire status` and IsEnabled read, status actually
// reported ENABLED — the effective state was correct and only the committed
// file was stale. That still bites anyone without the local file (a fresh
// clone, a teammate) and leaves the committed source of truth wrong.
//
// This drives the real entire binary end-to-end — enable, disable --project,
// then a setup-flag re-enable — and asserts the PROJECT settings.json (the file
// the user actually disabled) is enabled again.
func TestEnableReenablesProjectScopeAfterProjectDisable(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	// First-time setup via the real binary; a plain enable writes the project
	// .entire/settings.json.
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	assertProjectSettingsEnabled(t, env, true)

	// Disable at the project scope → settings.json enabled=false.
	env.RunCLI("disable", "--project")
	assertProjectSettingsEnabled(t, env, false)

	// Re-enable with a setup flag but WITHOUT --project/--local. Pre-fix the
	// enabled flag landed in settings.local.json, so the project file the user
	// disabled stayed enabled=false.
	env.RunCLI("enable", "--checkpoint-remote", "github:org/repo", "--skip-push-sessions", "--telemetry=false")

	assertProjectSettingsEnabled(t, env, true)

	// And no local override may contradict it: settings.local.json must be
	// absent or itself enabled:true, so the merged view can't silently flip
	// back to disabled by accident of the flow.
	assertLocalSettingsAbsentOrEnabled(t, env)
}

// assertLocalSettingsAbsentOrEnabled asserts that .entire/settings.local.json,
// if present, does not carry an enabled:false override that would mask the
// committed project scope.
func assertLocalSettingsAbsentOrEnabled(t *testing.T, env *TestEnv) {
	t.Helper()
	localPath := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	data, err := os.ReadFile(localPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read .entire/settings.local.json: %v", err)
	}
	var s struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse .entire/settings.local.json: %v\ncontent: %s", err, data)
	}
	if s.Enabled != nil && !*s.Enabled {
		t.Fatalf("settings.local.json carries enabled:false, which would mask the re-enabled project scope\ncontent: %s", data)
	}
}

// assertProjectSettingsEnabled reads .entire/settings.json (the project scope,
// never settings.local.json) and asserts its enabled flag matches want.
func assertProjectSettingsEnabled(t *testing.T, env *TestEnv, want bool) {
	t.Helper()
	settingsPath := filepath.Join(env.RepoDir, ".entire", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read .entire/settings.json: %v", err)
	}
	var s struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse .entire/settings.json: %v\ncontent: %s", err, data)
	}
	if s.Enabled != want {
		t.Fatalf("project settings.json enabled=%v, want %v — enabled flag written to the wrong scope\ncontent: %s",
			s.Enabled, want, data)
	}
}
