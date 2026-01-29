//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/paths"
)

// RunEnableWithAccessibleMode runs `entire enable` without --strategy flag in accessible mode.
// It provides stdin input to answer the telemetry and shell completion prompts.
func (env *TestEnv) RunEnableWithAccessibleMode() string {
	env.T.Helper()

	// Run CLI with ACCESSIBLE=1 for non-interactive prompts
	// Provide "no" for telemetry and "no" for shell completion
	cmd := exec.Command(getTestBinary(), "enable")
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(), "ACCESSIBLE=1")
	// Provide input for prompts: "no" for telemetry, "no" for shell completion
	cmd.Stdin = strings.NewReader("no\nno\n")

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
	RunForAllStrategiesWithBasicEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Initially should be enabled (default)
		stdout := env.RunCLI("status")
		if !strings.Contains(stdout, "Enabled") {
			t.Errorf("Expected status to show 'Enabled', got: %s", stdout)
		}

		// Disable
		stdout = env.RunCLI("disable")
		if !strings.Contains(stdout, "disabled") {
			t.Errorf("Expected disable output to contain 'disabled', got: %s", stdout)
		}

		// Check status is now disabled
		stdout = env.RunCLI("status")
		if !strings.Contains(stdout, "Disabled") {
			t.Errorf("Expected status to show 'Disabled', got: %s", stdout)
		}

		// Re-enable (using --strategy flag for non-interactive mode)
		stdout = env.RunCLI("enable", "--strategy", strategyName)
		if !strings.Contains(stdout, "strategy enabled") {
			t.Errorf("Expected enable output to contain 'strategy enabled', got: %s", stdout)
		}

		// Check status is now enabled
		stdout = env.RunCLI("status")
		if !strings.Contains(stdout, "Enabled") {
			t.Errorf("Expected status to show 'Enabled', got: %s", stdout)
		}
	})
}

func TestRewindBlockedWhenDisabled(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Disable Entire
		env.SetEnabled(false)

		// Try to run rewind --list - should show disabled message (not error)
		stdout, err := env.RunCLIWithError("rewind", "--list")
		if err != nil {
			t.Fatalf("rewind --list command failed unexpectedly: %v\nOutput: %s", err, stdout)
		}
		if !strings.Contains(stdout, "Entire is disabled") {
			t.Errorf("Expected disabled message, got: %s", stdout)
		}
		if !strings.Contains(stdout, "entire enable") {
			t.Errorf("Expected message to mention 'entire enable', got: %s", stdout)
		}
	})
}

func TestHooksSilentWhenDisabled(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
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
	})
}

func TestStatusWhenDisabled(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithBasicEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Disable Entire
		env.SetEnabled(false)

		// Status command should still work and show disabled
		stdout := env.RunCLI("status")
		if !strings.Contains(stdout, "Disabled") {
			t.Errorf("Expected status to show 'Disabled', got: %s", stdout)
		}
	})
}

func TestEnableWhenDisabled(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithBasicEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Disable Entire
		env.SetEnabled(false)

		// Enable command should work (using --strategy flag for non-interactive mode)
		stdout := env.RunCLI("enable", "--strategy", strategyName)
		if !strings.Contains(stdout, "strategy enabled") {
			t.Errorf("Expected enable output to contain 'strategy enabled', got: %s", stdout)
		}

		// Verify it's now enabled
		stdout = env.RunCLI("status")
		if !strings.Contains(stdout, "Enabled") {
			t.Errorf("Expected status to show 'Enabled' after re-enabling, got: %s", stdout)
		}
	})
}

func TestEnableDefaultStrategy(t *testing.T) {
	t.Parallel()

	// Create a basic test environment with just a git repo (no Entire init)
	env := NewTestEnv(t)
	env.InitRepo()

	// Run entire enable without --strategy flag
	// This tests that the default strategy is manual-commit
	// We use stdin to answer the telemetry and shell completion prompts
	stdout := env.RunEnableWithAccessibleMode()

	// Verify output mentions manual-commit strategy
	if !strings.Contains(stdout, "manual-commit") {
		t.Errorf("Expected output to mention 'manual-commit' strategy, got: %s", stdout)
	}

	// Verify settings file has manual-commit strategy
	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("Failed to read settings file: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Failed to parse settings: %v", err)
	}

	strategy, ok := settings["strategy"].(string)
	if !ok {
		t.Fatalf("Strategy not found in settings: %v", settings)
	}

	if strategy != "manual-commit" {
		t.Errorf("Expected default strategy to be 'manual-commit', got: %s", strategy)
	}

	// Also verify via status command
	stdout = env.RunCLI("status")
	if !strings.Contains(stdout, "manual-commit") {
		t.Errorf("Expected status to show 'manual-commit', got: %s", stdout)
	}
}
