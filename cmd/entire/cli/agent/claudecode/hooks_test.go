package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// metadataDenyRuleTest is the rule that blocks Claude from reading Entire metadata
const metadataDenyRuleTest = "Read(./.entire/metadata/**)"

func TestInstallHooks_PermissionsDeny_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify permissions.deny contains our rule
	if !containsDenyRule(perms.Deny, metadataDenyRuleTest) {
		t.Errorf("permissions.deny = %v, want to contain %q", perms.Deny, metadataDenyRuleTest)
	}
}

func TestInstallHooks_PermissionsDeny_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}
	// First install
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Second install
	_, err = agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Count occurrences of our rule
	count := 0
	for _, rule := range perms.Deny {
		if rule == metadataDenyRuleTest {
			count++
		}
	}
	if count != 1 {
		t.Errorf("permissions.deny contains %d copies of rule, want 1", count)
	}
}

func TestInstallHooks_PermissionsDeny_PreservesUserRules(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with existing user deny rule
	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["Bash(rm -rf *)"]
  }
}`)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify both rules exist
	if !containsDenyRule(perms.Deny, "Bash(rm -rf *)") {
		t.Errorf("permissions.deny = %v, want to contain user rule", perms.Deny)
	}
	if !containsDenyRule(perms.Deny, metadataDenyRuleTest) {
		t.Errorf("permissions.deny = %v, want to contain Entire rule", perms.Deny)
	}
}

func TestInstallHooks_PermissionsDeny_PreservesAllowRules(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with existing allow rules
	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "allow": ["Read(**)", "Write(**)"]
  }
}`)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify allow rules are preserved
	if len(perms.Allow) != 2 {
		t.Errorf("permissions.allow = %v, want 2 rules", perms.Allow)
	}
	if !containsDenyRule(perms.Allow, "Read(**)") {
		t.Errorf("permissions.allow = %v, want to contain Read(**)", perms.Allow)
	}
	if !containsDenyRule(perms.Allow, "Write(**)") {
		t.Errorf("permissions.allow = %v, want to contain Write(**)", perms.Allow)
	}
}

func TestInstallHooks_PermissionsDeny_SkipsExistingRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with the rule already present
	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["Read(./.entire/metadata/**)"]
  }
}`)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Should still have exactly 1 rule
	if len(perms.Deny) != 1 {
		t.Errorf("permissions.deny = %v, want exactly 1 rule", perms.Deny)
	}
}

func TestInstallHooks_PermissionsDeny_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with unknown permission fields like "ask"
	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "allow": ["Read(**)"],
    "ask": ["Write(**)", "Bash(*)"],
    "customField": {"nested": "value"}
  }
}`)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read raw settings to check for unknown fields
	settingsPath := filepath.Join(tempDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}

	var rawPermissions map[string]json.RawMessage
	if err := json.Unmarshal(rawSettings["permissions"], &rawPermissions); err != nil {
		t.Fatalf("failed to parse permissions: %v", err)
	}

	// Verify "ask" field is preserved
	if _, ok := rawPermissions["ask"]; !ok {
		t.Errorf("permissions.ask was not preserved, got keys: %v", getKeys(rawPermissions))
	}

	// Verify "customField" is preserved
	if _, ok := rawPermissions["customField"]; !ok {
		t.Errorf("permissions.customField was not preserved, got keys: %v", getKeys(rawPermissions))
	}

	// Verify the "ask" field content
	var askRules []string
	if err := json.Unmarshal(rawPermissions["ask"], &askRules); err != nil {
		t.Fatalf("failed to parse permissions.ask: %v", err)
	}
	if len(askRules) != 2 || askRules[0] != "Write(**)" || askRules[1] != "Bash(*)" {
		t.Errorf("permissions.ask = %v, want [Write(**), Bash(*)]", askRules)
	}

	// Verify the deny rule was added
	var denyRules []string
	if err := json.Unmarshal(rawPermissions["deny"], &denyRules); err != nil {
		t.Fatalf("failed to parse permissions.deny: %v", err)
	}
	if !containsDenyRule(denyRules, metadataDenyRuleTest) {
		t.Errorf("permissions.deny = %v, want to contain %q", denyRules, metadataDenyRuleTest)
	}

	// Verify "allow" is preserved
	var allowRules []string
	if err := json.Unmarshal(rawPermissions["allow"], &allowRules); err != nil {
		t.Fatalf("failed to parse permissions.allow: %v", err)
	}
	if len(allowRules) != 1 || allowRules[0] != "Read(**)" {
		t.Errorf("permissions.allow = %v, want [Read(**)]", allowRules)
	}
}

// Helper functions

// testPermissions is used only for test assertions
type testPermissions struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

func readPermissions(t *testing.T, tempDir string) testPermissions {
	t.Helper()
	settingsPath := filepath.Join(tempDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}

	var perms testPermissions
	if permRaw, ok := rawSettings["permissions"]; ok {
		if err := json.Unmarshal(permRaw, &perms); err != nil {
			t.Fatalf("failed to parse permissions: %v", err)
		}
	}
	return perms
}

func writeSettingsFile(t *testing.T, tempDir, content string) {
	t.Helper()
	claudeDir := filepath.Join(tempDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("failed to create .claude dir: %v", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}
}

func containsDenyRule(rules []string, rule string) bool {
	for _, r := range rules {
		if r == rule {
			return true
		}
	}
	return false
}

func getKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}

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

	agent := &ClaudeCodeAgent{}

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
	writeSettingsFile(t, tempDir, `{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo user hook"}]
      },
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]
      }
    ]
  }
}`)

	agent := &ClaudeCodeAgent{}
	err := agent.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)

	// Verify only user hooks remain
	if len(settings.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d after uninstall, want 1 (user only)", len(settings.Hooks.Stop))
	}

	// Verify it's the user hook
	if len(settings.Hooks.Stop) > 0 && len(settings.Hooks.Stop[0].Hooks) > 0 {
		if settings.Hooks.Stop[0].Hooks[0].Command != "echo user hook" {
			t.Error("user hook was removed during uninstall")
		}
	}
}

func TestUninstallHooks_RemovesDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}

	// First install (which adds the deny rule)
	_, err := agent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify deny rule was added
	perms := readPermissions(t, tempDir)
	if !containsDenyRule(perms.Deny, metadataDenyRuleTest) {
		t.Fatal("deny rule should be present after install")
	}

	// Uninstall
	err = agent.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify deny rule was removed
	perms = readPermissions(t, tempDir)
	if containsDenyRule(perms.Deny, metadataDenyRuleTest) {
		t.Error("deny rule should be removed after uninstall")
	}
}

func TestUninstallHooks_PreservesUserDenyRules(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with user deny rule and entire deny rule
	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["Bash(rm -rf *)", "Read(./.entire/metadata/**)"]
  },
  "hooks": {
    "Stop": [
      {
        "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]
      }
    ]
  }
}`)

	agent := &ClaudeCodeAgent{}
	err := agent.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify user deny rule is preserved
	if !containsDenyRule(perms.Deny, "Bash(rm -rf *)") {
		t.Errorf("user deny rule was removed, got: %v", perms.Deny)
	}

	// Verify entire deny rule is removed
	if containsDenyRule(perms.Deny, metadataDenyRuleTest) {
		t.Errorf("entire deny rule should be removed, got: %v", perms.Deny)
	}
}

// readClaudeSettings reads and parses the Claude Code settings file
func readClaudeSettings(t *testing.T, tempDir string) ClaudeSettings {
	t.Helper()
	return readClaudeSettingsFile(t, tempDir, "settings.json")
}

func readClaudeSettingsFile(t *testing.T, tempDir, fileName string) ClaudeSettings {
	t.Helper()
	settingsPath := filepath.Join(tempDir, ".claude", fileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", fileName, err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse %s: %v", fileName, err)
	}
	return settings
}

func TestInstallHooksTo_WritesToLocalFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &ClaudeCodeAgent{}
	count, err := ag.InstallHooksTo(false, false, ClaudeSettingsLocalFileName)
	if err != nil {
		t.Fatalf("InstallHooksTo() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallHooksTo() installed 0 hooks, want > 0")
	}

	// Verify settings.local.json was created with hooks
	settings := readClaudeSettingsFile(t, tempDir, ClaudeSettingsLocalFileName)
	if !hookCommandExists(settings.Hooks.Stop, "entire hooks claude-code stop") {
		t.Error("stop hook not found in settings.local.json")
	}

	// Verify settings.json was NOT created
	settingsPath := filepath.Join(tempDir, ".claude", ClaudeSettingsFileName)
	if _, err := os.Stat(settingsPath); err == nil {
		t.Error("settings.json should not exist when installing to local file")
	}
}

func TestInstallHooksTo_DoesNotModifyProjectSettings(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &ClaudeCodeAgent{}

	// Install to project settings first
	_, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	projectData, err := os.ReadFile(filepath.Join(tempDir, ".claude", ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	// Now install to local settings
	_, err = ag.InstallHooksTo(false, false, ClaudeSettingsLocalFileName)
	if err != nil {
		t.Fatalf("InstallHooksTo() error = %v", err)
	}

	// Verify project settings unchanged
	projectDataAfter, err := os.ReadFile(filepath.Join(tempDir, ".claude", ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(projectData) != string(projectDataAfter) {
		t.Error("settings.json was modified when installing to local file")
	}
}
