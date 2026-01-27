//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/agent/geminicli"
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

	// Verify enableHooks is set
	if !settings.Tools.EnableHooks {
		t.Error("tools.enableHooks should be true")
	}

	// Verify hooks.enabled is also set (both are required for hooks to run)
	if !settings.Hooks.Enabled {
		t.Error("hooks.enabled should be true")
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

// TestGeminiHooks_SessionStartUpdatesSessionID verifies that the session-start
// hook updates the current session ID with a date prefix and writes it to the
// .entire/current_session file.
func TestGeminiHooks_SessionStartUpdatesSessionID(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	// Create initial commit
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Prepare session-start hook input (JSON that Gemini CLI sends to the hook)
	geminiSessionID := "test-gemini-session-abc123"

	// Use json.Marshal to properly escape the path for cross-platform compatibility
	cwdJSON, err := json.Marshal(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to marshal repo dir: %v", err)
	}

	hookInput := `{
  "session_id": "` + geminiSessionID + `",
  "transcript_path": "/path/to/transcript.json",
  "cwd": ` + string(cwdJSON) + `,
  "hook_event_name": "SessionStart",
  "timestamp": "2024-01-01T12:00:00Z",
  "source": "startup"
}`

	// Run the session-start hook with stdin
	output := env.RunCLIWithStdin(hookInput, "hooks", "gemini", "session-start")

	// Verify output confirms session was set
	if !strings.Contains(output, "Current session set to:") {
		t.Errorf("expected output to confirm session was set, got: %s", output)
	}

	// Read the current_session file to verify the session ID was written
	sessionFile := filepath.Join(env.RepoDir, ".entire", "current_session")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatalf("failed to read current_session file: %v", err)
	}

	currentSessionID := strings.TrimSpace(string(data))

	// Verify the session ID has the date prefix (format: YYYY-MM-DD-<session-id>)
	expectedSuffix := "-" + geminiSessionID
	if !strings.HasSuffix(currentSessionID, expectedSuffix) {
		t.Errorf("current session ID = %q, should end with %q", currentSessionID, expectedSuffix)
	}

	// Verify the date prefix is present (11 chars: YYYY-MM-DD-)
	if len(currentSessionID) < 11+len(geminiSessionID) {
		t.Fatalf("current session ID = %q, too short for date prefix", currentSessionID)
	}

	// Verify the date prefix format (YYYY-MM-DD-)
	datePrefix := currentSessionID[:11]
	if len(datePrefix) != 11 || datePrefix[4] != '-' || datePrefix[7] != '-' || datePrefix[10] != '-' {
		t.Errorf("current session ID = %q, date prefix %q doesn't match format YYYY-MM-DD-", currentSessionID, datePrefix)
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
