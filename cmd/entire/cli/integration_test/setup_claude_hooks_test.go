//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
)

// ClaudeSettings mirrors the settings structure for testing
type ClaudeSettings struct {
	Hooks struct {
		SessionStart     []HookMatcher `json:"SessionStart"`
		Stop             []HookMatcher `json:"Stop"`
		UserPromptSubmit []HookMatcher `json:"UserPromptSubmit"`
		PreToolUse       []HookMatcher `json:"PreToolUse"`
		PostToolUse      []HookMatcher `json:"PostToolUse"`
	} `json:"hooks"`
}

type HookMatcher struct {
	Matcher string `json:"matcher"`
	Hooks   []Hook `json:"hooks"`
}

type Hook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// TestSetupClaudeHooks_AddsAllRequiredHooks is a smoke test verifying that
// `entire enable --agent claude-code` adds all required hooks to the correct file.
// Detailed hook manipulation logic is tested in unit tests (setup_test.go).
func TestSetupClaudeHooks_AddsAllRequiredHooks(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire() // Sets up .entire/settings.json

	// Create initial commit (required for setup)
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Run entire enable --agent claude-code (non-interactive)
	output, err := env.RunCLIWithError("enable", "--agent", "claude-code")
	if err != nil {
		t.Fatalf("enable claude-hooks command failed: %v\nOutput: %s", err, output)
	}

	// Read the generated settings.json
	settings := readClaudeSettings(t, env)

	// Verify all 6 hooks exist
	if len(settings.Hooks.SessionStart) == 0 {
		t.Error("SessionStart hook should exist")
	}
	if len(settings.Hooks.Stop) == 0 {
		t.Error("Stop hook should exist")
	}
	if len(settings.Hooks.UserPromptSubmit) == 0 {
		t.Error("UserPromptSubmit hook should exist")
	}
	if !hasHookWithMatcher(settings.Hooks.PreToolUse, "Task") {
		t.Error("PreToolUse[Task] hook should exist")
	}
	if !hasHookWithMatcher(settings.Hooks.PostToolUse, "Task") {
		t.Error("PostToolUse[Task] hook should exist")
	}
	if !hasHookWithMatcher(settings.Hooks.PostToolUse, "TodoWrite") {
		t.Error("PostToolUse[TodoWrite] hook should exist")
	}

	searchAgentPath := filepath.Join(env.RepoDir, ".claude", "agents", "entire-search.md")
	data, err := os.ReadFile(searchAgentPath)
	if err != nil {
		t.Fatalf("failed to read generated Claude search subagent: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "ENTIRE-MANAGED SEARCH SUBAGENT") {
		t.Error("Claude search subagent should be marked as Entire-managed")
	}
	if !strings.Contains(content, "entire search --json") {
		t.Error("Claude search subagent should instruct use of `entire search --json`")
	}
}

// TestSetupClaudeHooks_PreservesExistingSettings is a smoke test verifying that
// enable claude-hooks doesn't nuke existing settings or user-configured hooks.
// Detailed preservation logic is tested in unit tests (setup_test.go).
func TestSetupClaudeHooks_PreservesExistingSettings(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create existing settings with custom fields and user hooks
	claudeDir := filepath.Join(env.RepoDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("failed to create .claude dir: %v", err)
	}

	existingSettings := `{
  "customSetting": "should-be-preserved",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Task",
        "hooks": [{"type": "command", "command": "echo user-task-hook"}]
      },
      {
        "matcher": "CustomTool",
        "hooks": [{"type": "command", "command": "echo custom"}]
      }
    ]
  }
}`
	settingsPath := filepath.Join(claudeDir, claudecode.ClaudeSettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(existingSettings), 0o644); err != nil {
		t.Fatalf("failed to write existing settings: %v", err)
	}

	// Run enable claude-hooks
	output, err := env.RunCLIWithError("enable", "--agent", "claude-code")
	if err != nil {
		t.Fatalf("enable claude-hooks failed: %v\nOutput: %s", err, output)
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
		t.Error("customSetting should be preserved after enable claude-hooks")
	}

	// Verify user hooks are preserved
	settings := readClaudeSettings(t, env)

	// User's CustomTool hook should still exist
	if !hasHookWithMatcher(settings.Hooks.PreToolUse, "CustomTool") {
		t.Error("existing CustomTool hook should be preserved")
	}

	// User's Task hook should be preserved alongside our hook
	taskHooks := getAllHookCommands(settings.Hooks.PreToolUse, "Task")
	if !containsCommand(taskHooks, "echo user-task-hook") {
		t.Errorf("user's Task hook should be preserved, got: %v", taskHooks)
	}

	// Our hooks should also be added
	if !hasHookWithMatcher(settings.Hooks.PostToolUse, "Task") {
		t.Error("PostToolUse[Task] hook should be added")
	}
}

// Helper functions

func readClaudeSettings(t *testing.T, env *TestEnv) ClaudeSettings {
	t.Helper()
	settingsPath := filepath.Join(env.RepoDir, ".claude", claudecode.ClaudeSettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read %s at %s: %v", claudecode.ClaudeSettingsFileName, settingsPath, err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	return settings
}

func hasHookWithMatcher(matchers []HookMatcher, matcherPattern string) bool {
	for _, m := range matchers {
		if m.Matcher == matcherPattern && len(m.Hooks) > 0 {
			return true
		}
	}
	return false
}

func getAllHookCommands(matchers []HookMatcher, matcherPattern string) []string {
	var commands []string
	for _, m := range matchers {
		if m.Matcher == matcherPattern {
			for _, h := range m.Hooks {
				commands = append(commands, h.Command)
			}
		}
	}
	return commands
}

func containsCommand(commands []string, target string) bool {
	for _, cmd := range commands {
		if cmd == target {
			return true
		}
	}
	return false
}
