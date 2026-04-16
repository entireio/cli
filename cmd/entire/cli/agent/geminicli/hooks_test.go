package geminicli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

const testMatcherStartup = "startup"
const testHookNameMyHook = "my-hook"

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}
	count, err := agent.InstallHooks(context.Background(), false, false)
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

	// Verify HooksConfig.Enabled is true
	if !settings.HooksConfig.Enabled {
		t.Error("hooksConfig.enabled should be true")
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

	// Verify hook commands (localDev=false, so use entire binary)
	verifyHookCommand(t, settings.Hooks.SessionStart, "", agentpkg.WrapProductionJSONWarningHookCommand("entire hooks gemini session-start", agentpkg.WarningFormatSingleLine))
	verifyHookCommand(t, settings.Hooks.SessionEnd, "exit", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini session-end"))
	verifyHookCommand(t, settings.Hooks.SessionEnd, "logout", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini session-end"))
	verifyHookCommand(t, settings.Hooks.BeforeAgent, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini before-agent"))
	verifyHookCommand(t, settings.Hooks.AfterAgent, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini after-agent"))
	verifyHookCommand(t, settings.Hooks.BeforeModel, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini before-model"))
	verifyHookCommand(t, settings.Hooks.AfterModel, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini after-model"))
	verifyHookCommand(t, settings.Hooks.BeforeToolSelection, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini before-tool-selection"))
	verifyHookCommand(t, settings.Hooks.BeforeTool, "*", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini before-tool"))
	verifyHookCommand(t, settings.Hooks.AfterTool, "*", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini after-tool"))
	verifyHookCommand(t, settings.Hooks.PreCompress, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini pre-compress"))
	verifyHookCommand(t, settings.Hooks.Notification, "", agentpkg.WrapProductionSilentHookCommand("entire hooks gemini notification"))
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readGeminiSettings(t, tempDir)

	// Verify local dev commands use git rev-parse for runtime repo root resolution
	prefix := `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks gemini `
	verifyHookCommand(t, settings.Hooks.SessionStart, "", prefix+"session-start")
	verifyHookCommand(t, settings.Hooks.SessionEnd, "exit", prefix+"session-end")
	verifyHookCommand(t, settings.Hooks.SessionEnd, "logout", prefix+"session-end")
	verifyHookCommand(t, settings.Hooks.BeforeAgent, "", prefix+"before-agent")
	verifyHookCommand(t, settings.Hooks.AfterAgent, "", prefix+"after-agent")
	verifyHookCommand(t, settings.Hooks.BeforeModel, "", prefix+"before-model")
	verifyHookCommand(t, settings.Hooks.AfterModel, "", prefix+"after-model")
	verifyHookCommand(t, settings.Hooks.BeforeToolSelection, "", prefix+"before-tool-selection")
	verifyHookCommand(t, settings.Hooks.PreCompress, "", prefix+"pre-compress")
	verifyHookCommand(t, settings.Hooks.Notification, "", prefix+"notification")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// First install
	count1, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 12 {
		t.Errorf("first InstallHooks() count = %d, want 12", count1)
	}

	// Second install should add 0 hooks
	count2, err := agent.InstallHooks(context.Background(), false, false)
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
	_, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Force reinstall should replace hooks
	count, err := agent.InstallHooks(context.Background(), false, true)
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
	_, err := agent.InstallHooks(context.Background(), false, false)
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
		if matcher.Matcher == testMatcherStartup {
			for _, hook := range matcher.Hooks {
				if hook.Name == testHookNameMyHook {
					foundUserHook = true
				}
			}
		}
	}
	if !foundUserHook {
		t.Error("user hook 'my-hook' was not preserved")
	}
}

func TestInstallHooks_PreservesUnknownHookTypes(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with hook types we don't handle (hypothetical future Gemini hook types)
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "FutureHook": [
      {
        "matcher": "",
        "hooks": [{"name": "future-hook", "type": "command", "command": "echo future"}]
      }
    ],
    "AnotherNewHook": [
      {
        "matcher": "pattern",
        "hooks": [{"name": "another-hook", "type": "command", "command": "echo another"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read raw hooks to verify unknown hook types are preserved
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")

	// Verify FutureHook is preserved
	if _, ok := rawHooks["FutureHook"]; !ok {
		t.Errorf("FutureHook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify AnotherNewHook is preserved
	if _, ok := rawHooks["AnotherNewHook"]; !ok {
		t.Errorf("AnotherNewHook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify the FutureHook content is intact
	var futureMatchers []GeminiHookMatcher
	if err := json.Unmarshal(rawHooks["FutureHook"], &futureMatchers); err != nil {
		t.Fatalf("failed to parse FutureHook: %v", err)
	}
	if len(futureMatchers) != 1 {
		t.Errorf("FutureHook matchers = %d, want 1", len(futureMatchers))
	}
	if len(futureMatchers) > 0 && len(futureMatchers[0].Hooks) > 0 {
		if futureMatchers[0].Hooks[0].Command != "echo future" {
			t.Errorf("FutureHook command = %q, want %q",
				futureMatchers[0].Hooks[0].Command, "echo future")
		}
	}

	// Verify AnotherNewHook content including matcher
	var anotherMatchers []GeminiHookMatcher
	if err := json.Unmarshal(rawHooks["AnotherNewHook"], &anotherMatchers); err != nil {
		t.Fatalf("failed to parse AnotherNewHook: %v", err)
	}
	if len(anotherMatchers) > 0 {
		if anotherMatchers[0].Matcher != "pattern" {
			t.Errorf("AnotherNewHook matcher = %q, want %q", anotherMatchers[0].Matcher, "pattern")
		}
	}

	// Verify our hooks were also installed
	if _, ok := rawHooks["SessionStart"]; !ok {
		t.Errorf("SessionStart hook should have been installed")
	}
}

func TestUninstallHooks_PreservesUnknownHookTypes(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with Entire hooks AND unknown hook types
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [{"name": "entire-session-start", "type": "command", "command": "sh -c 'if ! command -v entire >/dev/null 2>&1; then echo \"Entire CLI is enabled but not installed or not on PATH. Installation guide: https://docs.entire.io/cli/installation#installation-methods\" >&2; exit 0; fi; exec entire hooks gemini session-start'"}]
      }
    ],
    "FutureHook": [
      {
        "matcher": "",
        "hooks": [{"name": "future-hook", "type": "command", "command": "echo future"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Read raw hooks to verify unknown hook types are preserved
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")

	// Verify FutureHook is preserved
	if _, ok := rawHooks["FutureHook"]; !ok {
		t.Errorf("FutureHook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify our hooks were removed (SessionStart should be empty/removed)
	if sessionStartRaw, ok := rawHooks["SessionStart"]; ok {
		var matchers []GeminiHookMatcher
		if err := json.Unmarshal(sessionStartRaw, &matchers); err == nil && len(matchers) > 0 {
			t.Errorf("SessionStart hook should have been removed")
		}
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
	_, err := agent.InstallHooks(context.Background(), false, false)
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
	_, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify hooks are installed
	if !agent.AreHooksInstalled(context.Background()) {
		t.Error("hooks should be installed before uninstall")
	}

	// Uninstall
	err = agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify hooks are removed
	if agent.AreHooksInstalled(context.Background()) {
		t.Error("hooks should not be installed after uninstall")
	}
}

func TestUninstallHooks_NoSettingsFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// Should not error when no settings file exists
	err := agent.UninstallHooks(context.Background())
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
        "hooks": [{"name": "entire-session-start", "type": "command", "command": "sh -c 'if ! command -v entire >/dev/null 2>&1; then echo \"Entire CLI is enabled but not installed or not on PATH. Installation guide: https://docs.entire.io/cli/installation#installation-methods\" >&2; exit 0; fi; exec entire hooks gemini session-start'"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	settings := readGeminiSettings(t, tempDir)

	// Verify only user hooks remain
	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d after uninstall, want 1 (user only)", len(settings.Hooks.SessionStart))
	}

	// Verify it's the user hook
	if settings.Hooks.SessionStart[0].Matcher != testMatcherStartup {
		t.Error("user hook was removed during uninstall")
	}
}

func TestAreHooksInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &GeminiCLIAgent{}

	// Should be false when no settings file
	if agent.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() should be false when no settings file")
	}

	// Install hooks
	_, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Should be true after installation
	if !agent.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() should be true after installation")
	}
}

func TestHookNames(t *testing.T) {
	agent := &GeminiCLIAgent{}
	names := agent.HookNames()

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
		t.Errorf("HookNames() returned %d names, want %d", len(names), len(expected))
	}

	for i, name := range expected {
		if names[i] != name {
			t.Errorf("HookNames()[%d] = %q, want %q", i, names[i], name)
		}
	}
}

func TestInstallHooks_RemovesLegacyEnabledField(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Simulate settings.json written by old Entire that put "enabled": true inside hooks
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "enabled": true,
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [{"name": "my-hook", "type": "command", "command": "echo user-startup-hook"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify "enabled" boolean is gone from hooks
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")
	if _, ok := rawHooks["enabled"]; ok {
		t.Error("legacy hooks.enabled field should have been removed")
	}

	// Verify the user hook in SessionStart is still present
	settings := readGeminiSettings(t, tempDir)
	foundUserHook := false
	for _, matcher := range settings.Hooks.SessionStart {
		if matcher.Matcher == testMatcherStartup {
			for _, hook := range matcher.Hooks {
				if hook.Name == testHookNameMyHook {
					foundUserHook = true
				}
			}
		}
	}
	if !foundUserHook {
		t.Error("user hook 'my-hook' should be preserved after legacy cleanup")
	}
}

func TestInstallHooks_RemovesLegacyEnabledField_WhenAlreadyInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Hooks already installed but legacy "enabled": true is also present
	writeGeminiSettings(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "enabled": true,
    "SessionStart": [
      {
        "hooks": [{"name": "entire-session-start", "type": "command", "command": %q}]
      }
    ]
  }
}`, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks gemini session-start", agentpkg.WarningFormatSingleLine)))

	agent := &GeminiCLIAgent{}
	n, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Hooks were already installed — cleanup-only run should return 0, not 12.
	if n != 0 {
		t.Errorf("InstallHooks() count = %d, want 0 (hooks already installed, only cleanup occurred)", n)
	}

	// Verify "enabled" boolean is gone even though idempotency would have fired
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")
	if _, ok := rawHooks["enabled"]; ok {
		t.Error("legacy hooks.enabled field should have been removed even when hooks were already installed")
	}
}

func TestInstallHooks_RemovesMultipleLegacyFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Multiple non-array legacy fields in hooks
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "enabled": true,
    "version": "1.0",
    "debug": false,
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [{"name": "my-hook", "type": "command", "command": "echo user-startup-hook"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")
	for _, key := range []string{"enabled", "version", "debug"} {
		if _, ok := rawHooks[key]; ok {
			t.Errorf("legacy field %q should have been removed", key)
		}
	}

	// Verify user hook survived
	settings := readGeminiSettings(t, tempDir)
	foundUserHook := false
	for _, matcher := range settings.Hooks.SessionStart {
		if matcher.Matcher == testMatcherStartup {
			for _, hook := range matcher.Hooks {
				if hook.Name == testHookNameMyHook {
					foundUserHook = true
				}
			}
		}
	}
	if !foundUserHook {
		t.Error("user hook 'my-hook' should be preserved after legacy cleanup")
	}
}

func TestInstallHooks_ForceWithLegacyFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Legacy field present with existing hooks, force reinstall
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "enabled": true,
    "SessionStart": [
      {
        "hooks": [{"name": "entire-session-start", "type": "command", "command": "sh -c 'if ! command -v entire >/dev/null 2>&1; then echo \"Entire CLI is enabled but not installed or not on PATH. Installation guide: https://docs.entire.io/cli/installation#installation-methods\" >&2; exit 0; fi; exec entire hooks gemini session-start'"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	count, err := agent.InstallHooks(context.Background(), false, true)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Force should reinstall all 12 hooks
	if count != 12 {
		t.Errorf("InstallHooks() count = %d, want 12 (force reinstall)", count)
	}

	// Legacy field should be gone
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")
	if _, ok := rawHooks["enabled"]; ok {
		t.Error("legacy hooks.enabled field should have been removed on force reinstall")
	}
}

func TestUninstallHooks_RemovesLegacyEnabledField(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Simulate legacy settings with "enabled": true inside hooks plus an Entire hook
	writeGeminiSettings(t, tempDir, `{
  "hooks": {
    "enabled": true,
    "SessionStart": [
      {
        "hooks": [{"name": "entire-session-start", "type": "command", "command": "sh -c 'if ! command -v entire >/dev/null 2>&1; then echo \"Entire CLI is enabled but not installed or not on PATH. Installation guide: https://docs.entire.io/cli/installation#installation-methods\" >&2; exit 0; fi; exec entire hooks gemini session-start'"}]
      }
    ]
  }
}`)

	agent := &GeminiCLIAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify "enabled" boolean is gone from hooks
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".gemini")
	if _, ok := rawHooks["enabled"]; ok {
		t.Error("legacy hooks.enabled field should have been removed by UninstallHooks")
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
