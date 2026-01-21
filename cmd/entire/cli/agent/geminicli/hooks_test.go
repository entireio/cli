package geminicli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}
	count, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// 12 hooks: SessionStart, SessionEnd (exit+logout), BeforeAgent, AfterAgent,
	// BeforeModel, AfterModel, BeforeToolSelection, BeforeTool, AfterTool, PreCompress, Notification
	if count != 12 {
		t.Errorf("InstallHooks() count = %d, want 12", count)
	}

	// Verify settings.json was created with hooks
	settings := readGeminiSettings(t, tempDir)

	// Verify enableHooks is true
	if !settings.Tools.EnableHooks {
		t.Error("tools.enableHooks should be true")
	}

	// Verify all hooks are present
	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d, want 1", len(settings.Hooks.SessionStart))
	}
	// SessionEnd has 2 matchers: exit and logout
	if len(settings.Hooks.SessionEnd) != 2 {
		t.Errorf("SessionEnd hooks = %d, want 2 (exit + logout)", len(settings.Hooks.SessionEnd))
	}
	if len(settings.Hooks.BeforeAgent) != 1 {
		t.Errorf("BeforeAgent hooks = %d, want 1", len(settings.Hooks.BeforeAgent))
	}
	if len(settings.Hooks.AfterAgent) != 1 {
		t.Errorf("AfterAgent hooks = %d, want 1", len(settings.Hooks.AfterAgent))
	}
	if len(settings.Hooks.BeforeTool) != 1 {
		t.Errorf("BeforeTool hooks = %d, want 1", len(settings.Hooks.BeforeTool))
	}
	if len(settings.Hooks.AfterTool) != 1 {
		t.Errorf("AfterTool hooks = %d, want 1", len(settings.Hooks.AfterTool))
	}
	if len(settings.Hooks.BeforeModel) != 1 {
		t.Errorf("BeforeModel hooks = %d, want 1", len(settings.Hooks.BeforeModel))
	}
	if len(settings.Hooks.AfterModel) != 1 {
		t.Errorf("AfterModel hooks = %d, want 1", len(settings.Hooks.AfterModel))
	}
	if len(settings.Hooks.BeforeToolSelection) != 1 {
		t.Errorf("BeforeToolSelection hooks = %d, want 1", len(settings.Hooks.BeforeToolSelection))
	}
	if len(settings.Hooks.PreCompress) != 1 {
		t.Errorf("PreCompress hooks = %d, want 1", len(settings.Hooks.PreCompress))
	}
	if len(settings.Hooks.Notification) != 1 {
		t.Errorf("Notification hooks = %d, want 1", len(settings.Hooks.Notification))
	}

	// Verify hook commands (always use go run)
	verifyHookCommand(t, settings.Hooks.SessionStart, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-start")
	verifyHookCommand(t, settings.Hooks.SessionEnd, "exit", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-end")
	verifyHookCommand(t, settings.Hooks.SessionEnd, "logout", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-end")
	verifyHookCommand(t, settings.Hooks.BeforeAgent, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-agent")
	verifyHookCommand(t, settings.Hooks.AfterAgent, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini after-agent")
	verifyHookCommand(t, settings.Hooks.BeforeModel, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-model")
	verifyHookCommand(t, settings.Hooks.AfterModel, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini after-model")
	verifyHookCommand(t, settings.Hooks.BeforeToolSelection, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-tool-selection")
	verifyHookCommand(t, settings.Hooks.BeforeTool, "*", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-tool")
	verifyHookCommand(t, settings.Hooks.AfterTool, "*", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini after-tool")
	verifyHookCommand(t, settings.Hooks.PreCompress, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini pre-compress")
	verifyHookCommand(t, settings.Hooks.Notification, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini notification")
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(true, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readGeminiSettings(t, tempDir)

	// Verify local dev commands use go run
	verifyHookCommand(t, settings.Hooks.SessionStart, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-start")
	verifyHookCommand(t, settings.Hooks.SessionEnd, "exit", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-end")
	verifyHookCommand(t, settings.Hooks.SessionEnd, "logout", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-end")
	verifyHookCommand(t, settings.Hooks.BeforeAgent, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-agent")
	verifyHookCommand(t, settings.Hooks.AfterAgent, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini after-agent")
	verifyHookCommand(t, settings.Hooks.BeforeModel, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-model")
	verifyHookCommand(t, settings.Hooks.AfterModel, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini after-model")
	verifyHookCommand(t, settings.Hooks.BeforeToolSelection, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini before-tool-selection")
	verifyHookCommand(t, settings.Hooks.PreCompress, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini pre-compress")
	verifyHookCommand(t, settings.Hooks.Notification, "", "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini notification")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// First install
	count1, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 12 {
		t.Errorf("first InstallHooks() count = %d, want 12", count1)
	}

	// Second install should add 0 hooks
	count2, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count2 != 0 {
		t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count2)
	}

	// Verify still only 1 hook per type (except SessionEnd which has 2 matchers)
	settings := readGeminiSettings(t, tempDir)
	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d after double install, want 1", len(settings.Hooks.SessionStart))
	}
	if len(settings.Hooks.SessionEnd) != 2 {
		t.Errorf("SessionEnd hooks = %d after double install, want 2", len(settings.Hooks.SessionEnd))
	}
}

func TestInstallHooks_Force(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// First install
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Force reinstall should replace hooks
	count, err := agent.InstallHooks(false, true)
	if err != nil {
		t.Fatalf("force InstallHooks() error = %v", err)
	}
	if count != 12 {
		t.Errorf("force InstallHooks() count = %d, want 12", count)
	}
}

func TestInstallHooks_PreservesUserHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with existing user hooks
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [{"name": "my-hook", "type": "command", "command": "echo hello"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readGeminiSettings(t, tempDir)

	// Verify user hooks are preserved
	if len(settings.Hooks.SessionStart) != 2 {
		t.Errorf("SessionStart hooks = %d, want 2 (user + entire)", len(settings.Hooks.SessionStart))
	}

	// Verify user hook is still there
	foundUserHook := false
	for _, matcher := range settings.Hooks.SessionStart {
		if matcher.Matcher == "startup" {
			for _, hook := range matcher.Hooks {
				if hook.Name == "my-hook" {
					foundUserHook = true
				}
			}
		}
	}
	if !foundUserHook {
		t.Error("user hook 'my-hook' was not preserved")
	}
}

func TestInstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with unknown fields
	writeGeminiSettings(t, tempDir, `{
  "someOtherField": "value",
  "customConfig": {"nested": true}
}`)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read raw settings to verify unknown fields are preserved
	settingsPath := filepath.Join(tempDir, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}

	if _, ok := rawSettings["someOtherField"]; !ok {
		t.Error("someOtherField was not preserved")
	}
	if _, ok := rawSettings["customConfig"]; !ok {
		t.Error("customConfig was not preserved")
	}
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// First install
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify hooks are installed
	if !agent.AreHooksInstalled() {
		t.Error("hooks should be installed before uninstall")
	}

	// Uninstall
	err = agent.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify hooks are removed
	if agent.AreHooksInstalled() {
		t.Error("hooks should not be installed after uninstall")
	}
}

func TestUninstallHooks_NoSettingsFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// Should not error when no settings file exists
	err := agent.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() should not error when no settings file: %v", err)
	}
}

func TestUninstallHooks_PreservesUserHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with both user and entire hooks
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [{"name": "my-hook", "type": "command", "command": "echo hello"}]
      },
      {
        "hooks": [{"name": "entire-session-start", "type": "command", "command": "entire hooks gemini session-start"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	err := agent.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	settings := readGeminiSettings(t, tempDir)

	// Verify only user hooks remain
	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d after uninstall, want 1 (user only)", len(settings.Hooks.SessionStart))
	}

	// Verify it's the user hook
	if settings.Hooks.SessionStart[0].Matcher != "startup" {
		t.Error("user hook was removed during uninstall")
	}
}

func TestAreHooksInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// Should be false when no settings file
	if agent.AreHooksInstalled() {
		t.Error("AreHooksInstalled() should be false when no settings file")
	}

	// Install hooks
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Should be true after installation
	if !agent.AreHooksInstalled() {
		t.Error("AreHooksInstalled() should be true after installation")
	}
}

func TestGetHookNames(t *testing.T) {
	agent := &GeminiCLIAgent{}
	names := agent.GetHookNames()

	expected := []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameBeforeAgent,
		HookNameAfterAgent,
		HookNameBeforeModel,
		HookNameAfterModel,
		HookNameBeforeToolSelection,
		HookNameBeforeTool,
		HookNameAfterTool,
		HookNamePreCompress,
		HookNameNotification,
	}

	if len(names) != len(expected) {
		t.Errorf("GetHookNames() returned %d names, want %d", len(names), len(expected))
	}

	for i, name := range expected {
		if names[i] != name {
			t.Errorf("GetHookNames()[%d] = %q, want %q", i, names[i], name)
		}
	}
}

// Helper functions

func readGeminiSettings(t *testing.T, tempDir string) GeminiSettings {
	t.Helper()
	settingsPath := filepath.Join(tempDir, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var settings GeminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	return settings
}

func writeGeminiSettings(t *testing.T, tempDir, content string) {
	t.Helper()
	geminiDir := filepath.Join(tempDir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o755); err != nil {
		t.Fatalf("failed to create .gemini dir: %v", err)
	}
	settingsPath := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}
}

func verifyHookCommand(t *testing.T, matchers []GeminiHookMatcher, expectedMatcher, expectedCommand string) {
	t.Helper()
	for _, matcher := range matchers {
		if matcher.Matcher == expectedMatcher {
			for _, hook := range matcher.Hooks {
				if hook.Command == expectedCommand {
					return // Found
				}
			}
		}
	}
	t.Errorf("hook with matcher=%q command=%q not found", expectedMatcher, expectedCommand)
}
