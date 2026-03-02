//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
)

// Use the real Factory types from the factoryaidroid package to avoid schema drift.
type FactorySettings = factoryaidroid.FactorySettings

// TestSetupFactoryAIHooks_AddsAllRequiredHooks is a smoke test verifying that
// `entire enable --agent factoryai-droid` adds all required hooks to the correct file.
func TestSetupFactoryAIHooks_AddsAllRequiredHooks(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire() // Sets up .entire/settings.json

	// Create initial commit (required for setup)
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Run entire enable --agent factoryai-droid (non-interactive)
	output, err := env.RunCLIWithError("enable", "--agent", "factoryai-droid")
	if err != nil {
		t.Fatalf("enable factoryai-droid command failed: %v\nOutput: %s", err, output)
	}

	// Read the generated settings.json
	settings := readFactorySettingsFile(t, env)

	// Verify all hooks exist (7 total)
	if len(settings.Hooks.SessionStart) == 0 {
		t.Error("SessionStart hook should exist")
	}
	if len(settings.Hooks.SessionEnd) == 0 {
		t.Error("SessionEnd hook should exist")
	}
	if len(settings.Hooks.Stop) == 0 {
		t.Error("Stop hook should exist")
	}
	if len(settings.Hooks.UserPromptSubmit) == 0 {
		t.Error("UserPromptSubmit hook should exist")
	}
	if len(settings.Hooks.PreToolUse) == 0 {
		t.Error("PreToolUse hook should exist")
	}
	if len(settings.Hooks.PostToolUse) == 0 {
		t.Error("PostToolUse hook should exist")
	}
	if len(settings.Hooks.PreCompact) == 0 {
		t.Error("PreCompact hook should exist")
	}

	// Verify permissions.deny contains metadata deny rule
	settingsPath := filepath.Join(env.RepoDir, ".factory", factoryaidroid.FactorySettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Read(./.entire/metadata/**)") {
		t.Error("settings.json should contain permissions.deny rule for .entire/metadata/**")
	}
}

// TestSetupFactoryAIHooks_PreservesExistingSettings is a smoke test verifying that
// enable factoryai-droid doesn't nuke existing settings or user-configured hooks.
func TestSetupFactoryAIHooks_PreservesExistingSettings(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create existing settings with custom fields and user hooks
	factoryDir := filepath.Join(env.RepoDir, ".factory")
	if err := os.MkdirAll(factoryDir, 0o755); err != nil {
		t.Fatalf("failed to create .factory dir: %v", err)
	}

	existingSettings := `{
  "customSetting": "should-be-preserved",
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo user-stop-hook"}]
      }
    ]
  }
}`
	settingsPath := filepath.Join(factoryDir, factoryaidroid.FactorySettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(existingSettings), 0o644); err != nil {
		t.Fatalf("failed to write existing settings: %v", err)
	}

	// Run enable factoryai-droid
	output, err := env.RunCLIWithError("enable", "--agent", "factoryai-droid")
	if err != nil {
		t.Fatalf("enable factoryai-droid failed: %v\nOutput: %s", err, output)
	}

	// Verify custom setting is preserved
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var rawSettings map[string]interface{}
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}

	if rawSettings["customSetting"] != "should-be-preserved" {
		t.Error("customSetting should be preserved after enable factoryai-droid")
	}

	// Verify user hooks are preserved
	settings := readFactorySettingsFile(t, env)

	// User's Stop hook should still exist alongside our hook
	foundUserHook := false
	for _, matcher := range settings.Hooks.Stop {
		for _, hook := range matcher.Hooks {
			if hook.Command == "echo user-stop-hook" {
				foundUserHook = true
			}
		}
	}
	if !foundUserHook {
		t.Error("existing user hook 'echo user-stop-hook' should be preserved")
	}

	// Our hooks should also be added
	if len(settings.Hooks.SessionStart) == 0 {
		t.Error("SessionStart hook should be added")
	}
	if len(settings.Hooks.UserPromptSubmit) == 0 {
		t.Error("UserPromptSubmit hook should be added")
	}
}

// Helper functions

func readFactorySettingsFile(t *testing.T, env *TestEnv) FactorySettings {
	t.Helper()
	settingsPath := filepath.Join(env.RepoDir, ".factory", factoryaidroid.FactorySettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read %s at %s: %v", factoryaidroid.FactorySettingsFileName, settingsPath, err)
	}

	var settings FactorySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	return settings
}
