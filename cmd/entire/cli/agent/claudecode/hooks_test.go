package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

// metadataDenyRuleTest is the rule that blocks Claude from reading Entire metadata
const metadataDenyRuleTest = "Read(./.entire/metadata/**)"

func TestInstallHooks_PermissionsDeny_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify permissions.deny contains our rule
	if !containsRule(perms.Deny, metadataDenyRuleTest) {
		t.Errorf("permissions.deny = %v, want to contain %q", perms.Deny, metadataDenyRuleTest)
	}
}

func TestInstallHooks_PermissionsDeny_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}
	// First install
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Second install
	_, err = agent.InstallHooks(context.Background(), false)
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
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify both rules exist
	if !containsRule(perms.Deny, "Bash(rm -rf *)") {
		t.Errorf("permissions.deny = %v, want to contain user rule", perms.Deny)
	}
	if !containsRule(perms.Deny, metadataDenyRuleTest) {
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
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify allow rules are preserved
	if len(perms.Allow) != 2 {
		t.Errorf("permissions.allow = %v, want 2 rules", perms.Allow)
	}
	if !containsRule(perms.Allow, "Read(**)") {
		t.Errorf("permissions.allow = %v, want to contain Read(**)", perms.Allow)
	}
	if !containsRule(perms.Allow, "Write(**)") {
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
	_, err := agent.InstallHooks(context.Background(), false)
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
	_, err := agent.InstallHooks(context.Background(), false)
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
		t.Errorf("permissions.ask was not preserved, got keys: %v", testutil.GetKeys(rawPermissions))
	}

	// Verify "customField" is preserved
	if _, ok := rawPermissions["customField"]; !ok {
		t.Errorf("permissions.customField was not preserved, got keys: %v", testutil.GetKeys(rawPermissions))
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
	if !containsRule(denyRules, metadataDenyRuleTest) {
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

func containsRule(rules []string, rule string) bool {
	return slices.Contains(rules, rule)
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}

	// First install
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify hooks are installed
	if !agent.AreHooksInstalled(context.Background()) {
		t.Error("hooks should be installed before uninstall")
	}
	settings := readClaudeSettings(t, tempDir)
	if !hasEntireHook(settings.Hooks.SubagentStop) {
		t.Fatal("SubagentStop hook should be installed before uninstall")
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

	// RemovesSubagentStop: `entire disable` must strip the SubagentStop hook
	// installed by InstallHooks — without threading it through UninstallHooks,
	// disabling Entire would leave this hook behind.
	t.Run("removes SubagentStop", func(t *testing.T) {
		settings := readClaudeSettings(t, tempDir)
		if hasEntireHook(settings.Hooks.SubagentStop) {
			t.Error("SubagentStop hook should be removed after uninstall")
		}
	})
}

func TestUninstallHooks_NoSettingsFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &ClaudeCodeAgent{}

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
	err := agent.UninstallHooks(context.Background())
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
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify deny rule was added
	perms := readPermissions(t, tempDir)
	if !containsRule(perms.Deny, metadataDenyRuleTest) {
		t.Fatal("deny rule should be present after install")
	}

	// Uninstall
	err = agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify deny rule was removed
	perms = readPermissions(t, tempDir)
	if containsRule(perms.Deny, metadataDenyRuleTest) {
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
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)

	// Verify user deny rule is preserved
	if !containsRule(perms.Deny, "Bash(rm -rf *)") {
		t.Errorf("user deny rule was removed, got: %v", perms.Deny)
	}

	// Verify entire deny rule is removed
	if containsRule(perms.Deny, metadataDenyRuleTest) {
		t.Errorf("entire deny rule should be removed, got: %v", perms.Deny)
	}
}

// TestInstallHooks_ReplacesLegacyLocalDevHook pins the migration for hooks left
// by the removed local-dev mode. Such a hook runs a script inside the working
// tree, so a plain install (no --force) must replace it, not add the binary hook
// alongside it — two hooks would both fire and the repo-relative one would keep
// executing whatever the checked-out branch contains.
func TestInstallHooks_ReplacesLegacyLocalDevHook(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ctx := context.Background()
	ag := &ClaudeCodeAgent{}

	legacyStop := testutil.LegacyClaudeProjectDirCommand("hooks claude-code stop")
	seedClaudeSettings(t, tempDir, legacyStop)

	// Sanity: the legacy shape must still be recognized as ours, otherwise it
	// would be treated as a foreign hook and deliberately left alone.
	if !isEntireHook(legacyStop) {
		t.Fatalf("legacy local-dev command not recognized as an Entire hook: %s", legacyStop)
	}

	if _, err := ag.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	var stopCmds []string
	for _, matcher := range settings.Hooks.Stop {
		for _, h := range matcher.Hooks {
			stopCmds = append(stopCmds, h.Command)
		}
	}

	wantStop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	if len(stopCmds) != 1 {
		t.Fatalf("Stop hooks = %d (%v), want exactly 1 — the legacy hook must be replaced, not joined", len(stopCmds), stopCmds)
	}
	if stopCmds[0] != wantStop {
		t.Errorf("Stop hook = %q, want %q", stopCmds[0], wantStop)
	}
	for _, cmd := range stopCmds {
		if strings.Contains(cmd, "scripts/entire-dev") {
			t.Errorf("a hook still points at a script inside the working tree: %s", cmd)
		}
	}
}

// TestInstallHooks_ReplacesLegacyLocalDevHookAlongsideCurrent covers the state a
// machine lands in if it installed hooks after the local-dev generation was
// removed but before the stale-hook cleanup existed: both a legacy and a current
// hook present. Nothing needs *adding* there, so an install that only writes
// when it added something would silently leave the legacy hook in place.
func TestInstallHooks_ReplacesLegacyLocalDevHookAlongsideCurrent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ctx := context.Background()
	ag := &ClaudeCodeAgent{}

	if _, err := ag.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	// Append a legacy hook next to the freshly installed current one.
	appendClaudeStopHook(t, tempDir, testutil.LegacyClaudeProjectDirCommand("hooks claude-code stop"))

	if _, err := ag.InstallHooks(ctx, false); err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	for _, matcher := range settings.Hooks.Stop {
		for _, h := range matcher.Hooks {
			if strings.Contains(h.Command, "scripts/entire-dev") {
				t.Errorf("legacy local-dev hook survived a non-force install: %s", h.Command)
			}
		}
	}
}

func TestUninstallHooks_RemovesLegacyLocalDevHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ctx := context.Background()
	ag := &ClaudeCodeAgent{}

	seedClaudeSettings(t, tempDir, testutil.LegacyClaudeProjectDirCommand("hooks claude-code stop"))
	if !ag.AreHooksInstalled(ctx) {
		t.Fatal("legacy local-dev hooks should be detected as installed")
	}
	if err := ag.UninstallHooks(ctx); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}
	if ag.AreHooksInstalled(ctx) {
		t.Fatal("legacy local-dev hooks should be removed after uninstall")
	}
}

// TestInstallHooks_SessionEndIsUntimed verifies no hook carries an explicit
// timeout. The explicit SessionEnd timeout existed only for local-dev hooks,
// which compiled from source and could exceed Claude Code's exit grace.
func TestInstallHooks_SessionEndIsUntimed(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	if len(settings.Hooks.SessionEnd) == 0 || len(settings.Hooks.SessionEnd[0].Hooks) == 0 {
		t.Fatal("expected a SessionEnd hook to be installed")
	}
	if got := settings.Hooks.SessionEnd[0].Hooks[0].Timeout; got != 0 {
		t.Errorf("SessionEnd timeout = %d, want 0", got)
	}

	// Stronger than the parsed check: prove the field is omitted entirely
	// (omitempty), not written as an explicit "timeout": 0.
	raw, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	if strings.Contains(string(raw), "timeout") {
		t.Errorf("settings.json must not contain any timeout field, got:\n%s", raw)
	}
}

// seedClaudeSettings writes a .claude/settings.json whose Stop hook is stopCmd.
func seedClaudeSettings(t *testing.T, dir, stopCmd string) {
	t.Helper()
	writeClaudeStopHooks(t, dir, []string{stopCmd})
}

// appendClaudeStopHook adds cmd to the existing Stop hooks, preserving them.
func appendClaudeStopHook(t *testing.T, dir, cmd string) {
	t.Helper()
	settings := readClaudeSettings(t, dir)
	var cmds []string
	for _, matcher := range settings.Hooks.Stop {
		for _, h := range matcher.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	writeClaudeStopHooks(t, dir, append(cmds, cmd))
}

func writeClaudeStopHooks(t *testing.T, dir string, cmds []string) {
	t.Helper()
	entries := make([]ClaudeHookEntry, 0, len(cmds))
	for _, c := range cmds {
		entries = append(entries, ClaudeHookEntry{Type: "command", Command: c})
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []ClaudeHookMatcher{{Matcher: "", Hooks: entries}},
		},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readClaudeSettings reads and parses the Claude Code settings file
func readClaudeSettings(t *testing.T, tempDir string) ClaudeSettings {
	t.Helper()
	settingsPath := filepath.Join(tempDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	return settings
}

//nolint:tparallel // Parent uses t.Chdir() which prevents t.Parallel(); subtests only read from pre-loaded data
func TestInstallHooks_PreservesUserHooksOnSameType(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with user hooks on the same hook types we use
	writeSettingsFile(t, tempDir, `{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo user stop hook"}]
      }
    ],
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo user session start"}]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Write",
        "hooks": [{"type": "command", "command": "echo user wrote file"}]
      }
    ]
  }
}`)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	rawHooks := testutil.ReadRawHooks(t, tempDir, ".claude")

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		var matchers []ClaudeHookMatcher
		if err := json.Unmarshal(rawHooks["Stop"], &matchers); err != nil {
			t.Fatalf("failed to parse Stop hooks: %v", err)
		}
		assertHookExists(t, matchers, "", "echo user stop hook", "user Stop hook")
		assertHookExists(t, matchers, "", agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop"), "Entire Stop hook")
	})

	t.Run("SessionStart", func(t *testing.T) {
		t.Parallel()
		var matchers []ClaudeHookMatcher
		if err := json.Unmarshal(rawHooks["SessionStart"], &matchers); err != nil {
			t.Fatalf("failed to parse SessionStart hooks: %v", err)
		}
		assertHookExists(t, matchers, "", "echo user session start", "user SessionStart hook")
		assertHookExists(t, matchers, "", agentpkg.WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", agentpkg.WarningFormatMultiLine), "Entire SessionStart hook")
	})

	t.Run("PostToolUse", func(t *testing.T) {
		t.Parallel()
		var matchers []ClaudeHookMatcher
		if err := json.Unmarshal(rawHooks["PostToolUse"], &matchers); err != nil {
			t.Fatalf("failed to parse PostToolUse hooks: %v", err)
		}
		assertHookExists(t, matchers, "Write", "echo user wrote file", "user Write hook")
		assertHookExists(t, matchers, "Agent", agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task"), "Entire Agent (subagent) hook")
		assertHookExists(t, matchers, "TaskCreate|TaskUpdate", agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo"), "Entire task-list hook")
	})
}

// assertHookExists checks that a hook with the given matcher and command exists
func assertHookExists(t *testing.T, matchers []ClaudeHookMatcher, matcher, command, description string) {
	t.Helper()
	for _, m := range matchers {
		if m.Matcher == matcher {
			for _, h := range m.Hooks {
				if h.Command == command {
					return
				}
			}
		}
	}
	t.Errorf("%s was not found (matcher=%q, command=%q)", description, matcher, command)
}

func TestInstallHooks_PreservesUnknownHookTypes(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with hook types we don't handle (Notification and
	// PreCompact are real Claude Code hook types). SubagentStop used to be the
	// second example here, but it is now a managed hook type (see
	// TestInstallHooks_SubagentStop_*), so PreCompact stands in for it.
	writeSettingsFile(t, tempDir, `{
  "hooks": {
    "Notification": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo notification received"}]
      }
    ],
    "PreCompact": [
      {
        "matcher": ".*",
        "hooks": [{"type": "command", "command": "echo pre compact"}]
      }
    ]
  }
}`)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read raw settings to check for unknown hook types
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".claude")

	// Verify Notification hook is preserved
	if _, ok := rawHooks["Notification"]; !ok {
		t.Errorf("Notification hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify PreCompact hook is preserved
	if _, ok := rawHooks["PreCompact"]; !ok {
		t.Errorf("PreCompact hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify the Notification hook content is intact
	var notificationMatchers []ClaudeHookMatcher
	if err := json.Unmarshal(rawHooks["Notification"], &notificationMatchers); err != nil {
		t.Fatalf("failed to parse Notification hooks: %v", err)
	}
	if len(notificationMatchers) != 1 {
		t.Errorf("Notification matchers = %d, want 1", len(notificationMatchers))
	}
	if len(notificationMatchers) > 0 && len(notificationMatchers[0].Hooks) > 0 {
		if notificationMatchers[0].Hooks[0].Command != "echo notification received" {
			t.Errorf("Notification hook command = %q, want %q",
				notificationMatchers[0].Hooks[0].Command, "echo notification received")
		}
	}

	// Verify the PreCompact hook content is intact
	var preCompactMatchers []ClaudeHookMatcher
	if err := json.Unmarshal(rawHooks["PreCompact"], &preCompactMatchers); err != nil {
		t.Fatalf("failed to parse PreCompact hooks: %v", err)
	}
	if len(preCompactMatchers) != 1 {
		t.Errorf("PreCompact matchers = %d, want 1", len(preCompactMatchers))
	}
	if len(preCompactMatchers) > 0 {
		if preCompactMatchers[0].Matcher != ".*" {
			t.Errorf("PreCompact matcher = %q, want %q", preCompactMatchers[0].Matcher, ".*")
		}
		if len(preCompactMatchers[0].Hooks) > 0 {
			if preCompactMatchers[0].Hooks[0].Command != "echo pre compact" {
				t.Errorf("PreCompact hook command = %q, want %q",
					preCompactMatchers[0].Hooks[0].Command, "echo pre compact")
			}
		}
	}

	// Verify our hooks were also installed
	if _, ok := rawHooks["Stop"]; !ok {
		t.Errorf("Stop hook should have been installed")
	}
}

func TestUninstallHooks_PreservesUnknownHookTypes(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with Entire hooks AND unknown hook types
	writeSettingsFile(t, tempDir, `{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]
      }
    ],
    "Notification": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo notification received"}]
      }
    ],
    "PreCompact": [
      {
        "matcher": ".*",
        "hooks": [{"type": "command", "command": "echo pre compact"}]
      }
    ]
  }
}`)

	agent := &ClaudeCodeAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Read raw settings to check for unknown hook types
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".claude")

	// Verify Notification hook is preserved
	if _, ok := rawHooks["Notification"]; !ok {
		t.Errorf("Notification hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify PreCompact hook is preserved
	if _, ok := rawHooks["PreCompact"]; !ok {
		t.Errorf("PreCompact hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify our hooks were removed
	if _, ok := rawHooks["Stop"]; ok {
		// Check if there are any hooks left (should be empty after uninstall)
		var stopMatchers []ClaudeHookMatcher
		if err := json.Unmarshal(rawHooks["Stop"], &stopMatchers); err == nil && len(stopMatchers) > 0 {
			t.Errorf("Stop hook should have been removed")
		}
	}
}

// TestInstallHooks_UsesCurrentToolMatchers pins the tool-use matchers to Claude
// Code's current tool names. The subagent tool is "Agent" (there was never a
// "Task" tool) and "TodoWrite" was deprecated for TaskCreate/TaskUpdate; a bad
// matcher is a silent no-op in Claude Code, so this is the only guard against
// the hooks silently ceasing to fire. See:
//   - https://code.claude.com/docs/en/tools-reference.md
//   - https://code.claude.com/docs/en/hooks.md
func TestInstallHooks_UsesCurrentToolMatchers(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	assertHookExists(t, settings.Hooks.PreToolUse, "Agent",
		agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code pre-task"), "pre-task subagent hook")
	assertHookExists(t, settings.Hooks.PostToolUse, "Agent",
		agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task"), "post-task subagent hook")
	assertHookExists(t, settings.Hooks.PostToolUse, "TaskCreate|TaskUpdate",
		agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo"), "post-todo task-list hook")

	// SubagentStop fresh install is the regression for wiring up the real
	// background-subagent-completion signal (SubagentStop fires at true
	// completion, even for background subagents whose PostToolUse fires
	// seconds after launch at the launch stub). A fresh install must write the
	// SubagentStop hook with the same empty-matcher, availability-guarded
	// `sh -c` shape as Stop.
	t.Run("SubagentStop fresh install", func(t *testing.T) {
		wantCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-stop")
		assertHookExists(t, settings.Hooks.SubagentStop, "", wantCmd, "SubagentStop hook")

		// Same shape as Stop: single command, empty matcher, no timeout.
		if len(settings.Hooks.SubagentStop) != 1 || settings.Hooks.SubagentStop[0].Matcher != "" {
			t.Fatalf("SubagentStop = %+v, want a single matcher with an empty string matcher (same shape as Stop)", settings.Hooks.SubagentStop)
		}
		if len(settings.Hooks.SubagentStop[0].Hooks) != 1 {
			t.Fatalf("SubagentStop hooks = %d, want 1", len(settings.Hooks.SubagentStop[0].Hooks))
		}
		if got := settings.Hooks.SubagentStop[0].Hooks[0].Timeout; got != 0 {
			t.Errorf("SubagentStop timeout = %d, want 0 (no explicit timeout, same as Stop)", got)
		}
	})
}

// TestInstallHooks_SubagentStop_UpgradeInPlace is the upgrade-path regression:
// a settings file written by a pre-SubagentStop CLI carries the seven other
// Entire hooks, current and healthy. A plain `entire enable` (InstallHooks
// with force=false) must repair it — add exactly the missing SubagentStop
// entry (count == 1) and leave every other hook type untouched. Existing
// installs must be repaired by enable, not just flagged by doctor forever.
func TestInstallHooks_SubagentStop_UpgradeInPlace(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &ClaudeCodeAgent{}

	// Produce a full current install, then delete the SubagentStop entry to
	// reproduce a settings file written by a pre-SubagentStop CLI. Using the
	// installer's own output (rather than a hand-written fixture) keeps the
	// other seven hook entries in exactly the shape a real old install left.
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("initial InstallHooks() error = %v", err)
	}
	settingsPath := filepath.Join(tempDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(rawSettings["hooks"], &rawHooks); err != nil {
		t.Fatalf("failed to parse hooks: %v", err)
	}
	if _, ok := rawHooks["SubagentStop"]; !ok {
		t.Fatal("precondition: full install must contain a SubagentStop entry")
	}
	delete(rawHooks, "SubagentStop")
	before := make(map[string]string, len(rawHooks))
	for hookType, raw := range rawHooks {
		before[hookType] = string(raw)
	}
	hooksJSON, err := json.Marshal(rawHooks)
	if err != nil {
		t.Fatalf("failed to marshal hooks: %v", err)
	}
	rawSettings["hooks"] = hooksJSON
	settingsJSON, err := json.Marshal(rawSettings)
	if err != nil {
		t.Fatalf("failed to marshal settings: %v", err)
	}
	if err := os.WriteFile(settingsPath, settingsJSON, 0o600); err != nil {
		t.Fatalf("failed to write pre-SubagentStop settings: %v", err)
	}

	count, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("upgrade InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Errorf("upgrade InstallHooks() count = %d, want exactly 1 (the SubagentStop entry)", count)
	}

	after := testutil.ReadRawHooks(t, tempDir, ".claude")
	wantCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-stop")
	var subagentStop []ClaudeHookMatcher
	if err := json.Unmarshal(after["SubagentStop"], &subagentStop); err != nil {
		t.Fatalf("failed to parse repaired SubagentStop entry: %v", err)
	}
	assertHookExists(t, subagentStop, "", wantCmd, "repaired SubagentStop hook")

	for hookType, beforeRaw := range before {
		afterRaw, ok := after[hookType]
		if !ok {
			t.Errorf("hook type %s disappeared during the upgrade", hookType)
			continue
		}
		if string(afterRaw) != beforeRaw {
			t.Errorf("hook type %s was rewritten during the upgrade:\nbefore: %s\nafter:  %s", hookType, beforeRaw, afterRaw)
		}
	}
}

// TestCheckHookConfig_Outdated_MissingSubagentStop pins CheckHookConfig's
// drift detection for the new hook: an install that predates SubagentStop
// (Stop + current tool-use matchers present, but no SubagentStop entry) must
// read as outdated so `entire doctor`/`entire enable --force` picks it up,
// not silently stay HooksCurrent forever.
func TestCheckHookConfig_Outdated_MissingSubagentStop(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	pre := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code pre-task")
	post := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task")
	todo := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q}]}],
    "PreToolUse": [{"matcher": "Agent", "hooks": [{"type": "command", "command": %q}]}],
    "PostToolUse": [
      {"matcher": "Agent", "hooks": [{"type": "command", "command": %q}]},
      {"matcher": "TaskCreate|TaskUpdate", "hooks": [{"type": "command", "command": %q}]}
    ]
  }
}`, stop, pre, post, todo))

	if got := CheckHookConfig(context.Background()); got != HooksOutdated {
		t.Errorf("CheckHookConfig() = %v, want HooksOutdated (missing SubagentStop)", got)
	}
}

func TestCheckHookConfig_Absent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	if got := CheckHookConfig(context.Background()); got != HooksAbsent {
		t.Errorf("CheckHookConfig() = %v, want HooksAbsent", got)
	}
}

func TestCheckHookConfig_Current(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if got := CheckHookConfig(context.Background()); got != HooksCurrent {
		t.Errorf("CheckHookConfig() = %v, want HooksCurrent", got)
	}
}

func TestCheckHookConfig_Outdated(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Config from an older CLI version: Entire installed (Stop present) but the
	// tool-use hooks sit under the outdated Task/TodoWrite matchers.
	stop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	pre := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code pre-task")
	post := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task")
	todo := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q}]}],
    "PreToolUse": [{"matcher": "Task", "hooks": [{"type": "command", "command": %q}]}],
    "PostToolUse": [
      {"matcher": "Task", "hooks": [{"type": "command", "command": %q}]},
      {"matcher": "TodoWrite", "hooks": [{"type": "command", "command": %q}]}
    ]
  }
}`, stop, pre, post, todo))

	if got := CheckHookConfig(context.Background()); got != HooksOutdated {
		t.Errorf("CheckHookConfig() = %v, want HooksOutdated", got)
	}
}

// TestCheckHookConfig_SupersetMatchersAreCurrent verifies that widening a
// matcher beyond what we install (still covering the required tools) is not
// flagged as drift — matchers are |-lists of exact tool names, so a superset
// still fires for the required tools.
func TestCheckHookConfig_SupersetMatchersAreCurrent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	subagentStop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-stop")
	pre := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code pre-task")
	post := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task")
	todo := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")
	// "Agent|Foo" still covers Agent; "TaskCreate|TaskUpdate|TaskGet" still
	// covers TaskCreate and TaskUpdate.
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q}]}],
    "SubagentStop": [{"matcher": "", "hooks": [{"type": "command", "command": %q}]}],
    "PreToolUse": [{"matcher": "Agent|Foo", "hooks": [{"type": "command", "command": %q}]}],
    "PostToolUse": [
      {"matcher": "Agent|Foo", "hooks": [{"type": "command", "command": %q}]},
      {"matcher": "TaskCreate|TaskUpdate|TaskGet", "hooks": [{"type": "command", "command": %q}]}
    ]
  }
}`, stop, subagentStop, pre, post, todo))

	if got := CheckHookConfig(context.Background()); got != HooksCurrent {
		t.Errorf("CheckHookConfig() = %v, want HooksCurrent (superset matcher)", got)
	}
}

// TestInstallHooks_Force_ReinstallsStaleToolMatchers verifies that `--force`
// strips Entire hooks left under the outdated "Task"/"TodoWrite" matchers by
// older CLI versions and reinstalls them under the current matchers. (A normal
// enable does not rewrite existing hook config; --force is the fix path.)
func TestInstallHooks_Force_ReinstallsStaleToolMatchers(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Settings written by an older CLI version, in the production wrapper form.
	stalePost := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task")
	staleTodo := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "PostToolUse": [
      {"matcher": "Task", "hooks": [{"type": "command", "command": %q}]},
      {"matcher": "TodoWrite", "hooks": [{"type": "command", "command": %q}]}
    ]
  }
}`, stalePost, staleTodo))

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), true); err != nil {
		t.Fatalf("InstallHooks(force) error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	// Reinstalled under the current matchers.
	assertHookExists(t, settings.Hooks.PostToolUse, "Agent",
		agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task"), "post-task hook")
	assertHookExists(t, settings.Hooks.PostToolUse, "TaskCreate|TaskUpdate",
		staleTodo, "post-todo hook")
	// The stale matchers no longer carry an Entire hook.
	for _, m := range settings.Hooks.PostToolUse {
		if m.Matcher == "Task" || m.Matcher == "TodoWrite" {
			for _, h := range m.Hooks {
				if isEntireHook(h.Command) {
					t.Errorf("stale matcher %q still carries an Entire hook: %q", m.Matcher, h.Command)
				}
			}
		}
	}
}

// TestCommittedDogfoodSettingsIsCurrent guards this repo's own committed agent config against drifting from what
// InstallHooks writes. A stale committed config is how the pi extension ended up
// invoking a launcher script that had been deleted.
func TestCommittedDogfoodSettingsIsCurrent(t *testing.T) {
	testutil.AssertCommittedDogfoodConfigStable(t, ".claude/settings.json", func(t *testing.T, dir string) (int, error) {
		t.Helper()
		t.Chdir(dir)
		return (&ClaudeCodeAgent{}).InstallHooks(context.Background(), false)
	})
}
