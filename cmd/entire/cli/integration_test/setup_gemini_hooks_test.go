//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
)

// Use the real Gemini types from the geminicli package to avoid schema drift.
type GeminiSettings = geminicli.GeminiSettings

// TestSetupGeminiHooks_AddsAllRequiredHooks is a smoke test verifying that
// `entire enable --agent gemini` adds all required hooks to the correct file.
func TestSetupGeminiHooks_AddsAllRequiredHooks(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit") // Sets up .entire/settings.json

	// Create initial commit (required for setup)
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Run entire enable --agent gemini (non-interactive)
	output, err := env.RunCLIWithError("enable", "--agent", "gemini")
	if err != nil {
		t.Fatalf("enable gemini command failed: %v\nOutput: %s", err, output)
	}

	// Read the generated settings.json
	settings := readGeminiSettingsFile(t, env)

	// Verify HooksConfig.Enabled is set
	if !settings.HooksConfig.Enabled {
		t.Error("hooksConfig.enabled should be true")
	}

	// Verify all hooks exist (12 total, but SessionEnd has 2 matchers)
	if len(settings.Hooks.SessionStart) == 0 {
		t.Error("SessionStart hook should exist")
	}
	if len(settings.Hooks.SessionEnd) < 2 {
		t.Errorf("SessionEnd hooks should have 2 matchers (exit + logout), got %d", len(settings.Hooks.SessionEnd))
	}
	if len(settings.Hooks.BeforeAgent) == 0 {
		t.Error("BeforeAgent hook should exist")
	}
	if len(settings.Hooks.AfterAgent) == 0 {
		t.Error("AfterAgent hook should exist")
	}
	if len(settings.Hooks.BeforeModel) == 0 {
		t.Error("BeforeModel hook should exist")
	}
	if len(settings.Hooks.AfterModel) == 0 {
		t.Error("AfterModel hook should exist")
	}
	if len(settings.Hooks.BeforeToolSelection) == 0 {
		t.Error("BeforeToolSelection hook should exist")
	}
	if len(settings.Hooks.BeforeTool) == 0 {
		t.Error("BeforeTool hook should exist")
	}
	if len(settings.Hooks.AfterTool) == 0 {
		t.Error("AfterTool hook should exist")
	}
	if len(settings.Hooks.PreCompress) == 0 {
		t.Error("PreCompress hook should exist")
	}
	if len(settings.Hooks.Notification) == 0 {
		t.Error("Notification hook should exist")
	}
}

// TestSetupGeminiHooks_LocalDevMode verifies that localDev mode uses wrapper script.
func TestSetupGeminiHooks_LocalDevMode(t *testing.T) {
	// Not parallel - uses os.Chdir
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	oldWd, _ := os.Getwd()
	if err := os.Chdir(env.RepoDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	// Manually install hooks with localDev=true
	gemini := geminicli.NewGeminiCLIAgent()
	hookAgent := gemini.(interface {
		InstallHooks(localDev bool, force bool) (int, error)
	})

	_, err := hookAgent.InstallHooks(true, false) // localDev = true
	if err != nil {
		t.Fatalf("InstallHooks(localDev=true) error = %v", err)
	}

	// Read settings and verify commands use wrapper script
	settingsPath := filepath.Join(env.RepoDir, ".gemini", geminicli.GeminiSettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "entire-wrapper.sh") {
		t.Error("localDev hooks should use wrapper script, but settings.json doesn't contain 'entire-wrapper.sh'")
	}
}

// TestSetupGeminiHooks_PreservesExistingSettings is a smoke test verifying that
// enable gemini doesn't nuke existing settings or user-configured hooks.
func TestSetupGeminiHooks_PreservesExistingSettings(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create existing settings with custom fields and user hooks
	geminiDir := filepath.Join(env.RepoDir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o755); err != nil {
		t.Fatalf("failed to create .gemini dir: %v", err)
	}

	existingSettings := `{
  "customSetting": "should-be-preserved",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [{"name": "my-hook", "type": "command", "command": "echo user-startup-hook"}]
      }
    ]
  }
}`
	settingsPath := filepath.Join(geminiDir, geminicli.GeminiSettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(existingSettings), 0o644); err != nil {
		t.Fatalf("failed to write existing settings: %v", err)
	}

	// Run enable gemini
	output, err := env.RunCLIWithError("enable", "--agent", "gemini")
	if err != nil {
		t.Fatalf("enable gemini failed: %v\nOutput: %s", err, output)
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
		t.Error("customSetting should be preserved after enable gemini")
	}

	// Verify user hooks are preserved
	settings := readGeminiSettingsFile(t, env)

	// User's startup matcher hook should still exist
	foundUserHook := false
	for _, matcher := range settings.Hooks.SessionStart {
		if matcher.Matcher == "startup" {
			for _, hook := range matcher.Hooks {
				if hook.Name == "my-hook" && hook.Command == "echo user-startup-hook" {
					foundUserHook = true
				}
			}
		}
	}
	if !foundUserHook {
		t.Error("existing user hook 'my-hook' should be preserved")
	}

	// Our hooks should also be added
	if len(settings.Hooks.AfterAgent) == 0 {
		t.Error("AfterAgent hook should be added")
	}
	if len(settings.Hooks.BeforeAgent) == 0 {
		t.Error("BeforeAgent hook should be added")
	}
}

// Helper functions

func readGeminiSettingsFile(t *testing.T, env *TestEnv) GeminiSettings {
	t.Helper()
	settingsPath := filepath.Join(env.RepoDir, ".gemini", geminicli.GeminiSettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read %s at %s: %v", geminicli.GeminiSettingsFileName, settingsPath, err)
	}

	var settings GeminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	return settings
}
