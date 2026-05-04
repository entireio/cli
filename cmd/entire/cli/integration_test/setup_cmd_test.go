//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// RunEnableWithAccessibleMode runs `entire enable` without --strategy flag in accessible mode.
// It provides stdin input to answer the telemetry prompt.
func (env *TestEnv) RunEnableWithAccessibleMode() string {
	env.T.Helper()

	// Run CLI with ACCESSIBLE=1 for non-interactive prompts
	// Provide "no" for telemetry
	cmd := exec.Command(getTestBinary(), "enable")
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
	stdout = env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	if !strings.Contains(stdout, "Ready.") {
		t.Errorf("Expected enable output to contain 'Ready.', got: %s", stdout)
	}

	// Check status is now enabled
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "Enabled") {
		t.Errorf("Expected status to show 'Enabled', got: %s", stdout)
	}
}

func TestRewindBlockedWhenDisabled(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	// Disable Entire
	env.SetEnabled(false)

	// Try to run checkpoint rewind --list - should show disabled message (not error)
	stdout, err := env.RunCLIWithError("checkpoint", "rewind", "--list")
	if err != nil {
		t.Fatalf("checkpoint rewind --list command failed unexpectedly: %v\nOutput: %s", err, stdout)
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
	stdout := env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
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

	// Verify status shows manual-commit (the only strategy)
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "manual-commit") {
		t.Errorf("Expected status to show 'manual-commit', got: %s", stdout)
	}
}
