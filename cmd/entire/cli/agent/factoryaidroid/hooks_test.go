package factoryaidroid

import (
	"context"
	"encoding/json"
	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &FactoryAIDroidAgent{}
	count, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// 8 hooks: SessionStart (session-start + user-prompt-submit), SessionEnd, Stop,
	// UserPromptSubmit, PreToolUse[Task], PostToolUse[Task], PreCompact
	if count != 8 {
		t.Errorf("InstallHooks() count = %d, want 8", count)
	}

	// Verify settings.json was created with hooks
	settings := readFactorySettings(t, tempDir)

	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d, want 1", len(settings.Hooks.SessionStart))
	}
	if len(settings.Hooks.SessionEnd) != 1 {
		t.Errorf("SessionEnd hooks = %d, want 1", len(settings.Hooks.SessionEnd))
	}
	if len(settings.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d, want 1", len(settings.Hooks.Stop))
	}
	if len(settings.Hooks.UserPromptSubmit) != 1 {
		t.Errorf("UserPromptSubmit hooks = %d, want 1", len(settings.Hooks.UserPromptSubmit))
	}
	if len(settings.Hooks.PreToolUse) != 1 {
		t.Errorf("PreToolUse hooks = %d, want 1", len(settings.Hooks.PreToolUse))
	}
	if len(settings.Hooks.PostToolUse) != 1 {
		t.Errorf("PostToolUse hooks = %d, want 1", len(settings.Hooks.PostToolUse))
	}
	if len(settings.Hooks.PreCompact) != 1 {
		t.Errorf("PreCompact hooks = %d, want 1", len(settings.Hooks.PreCompact))
	}

	// Verify hook commands
	assertFactoryHookExists(t, settings.Hooks.SessionStart, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-start"), "SessionStart")
	assertFactoryHookExists(t, settings.Hooks.SessionStart, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid user-prompt-submit"), "SessionStart user-prompt-submit")
	assertFactoryHookExists(t, settings.Hooks.SessionEnd, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-end"), "SessionEnd")
	assertFactoryHookExists(t, settings.Hooks.Stop, "", agentpkg.WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", agentpkg.WarningFormatSingleLine), "Stop")
	assertFactoryHookExists(t, settings.Hooks.UserPromptSubmit, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid user-prompt-submit"), "UserPromptSubmit")
	assertFactoryHookExists(t, settings.Hooks.PreToolUse, "Task", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid pre-tool-use"), "PreToolUse[Task]")
	assertFactoryHookExists(t, settings.Hooks.PostToolUse, "Task", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid post-tool-use"), "PostToolUse[Task]")
	assertFactoryHookExists(t, settings.Hooks.PreCompact, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid pre-compact"), "PreCompact")

	// Verify AreHooksInstalled returns true
	if !hooksInstalledNow(t, agent) {
		t.Error("AreHooksInstalled() should return true after install")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &FactoryAIDroidAgent{}

	// First install
	count1, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 8 {
		t.Errorf("first InstallHooks() count = %d, want 8", count1)
	}

	// Second install should add 0 hooks
	count2, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count2 != 0 {
		t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count2)
	}

	// Verify still only 1 matcher per hook type
	settings := readFactorySettings(t, tempDir)
	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d after double install, want 1", len(settings.Hooks.SessionStart))
	}
	if len(settings.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d after double install, want 1", len(settings.Hooks.Stop))
	}
}

func TestInstallHooks_ReplacesLegacyLocalDevHook(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}

	testutil.AssertLegacyHookReplaced(t,
		filepath.Join(tempDir, ".factory", "settings.json"),
		agentpkg.WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", agentpkg.WarningFormatSingleLine),
		testutil.LegacyLocalDevCommand("hooks factoryai-droid stop"),
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

func TestInstallHooks_Force(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &FactoryAIDroidAgent{}

	// First install
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Force reinstall should replace hooks
	count, err := agent.InstallHooks(context.Background(), true)
	if err != nil {
		t.Fatalf("force InstallHooks() error = %v", err)
	}
	if count != 8 {
		t.Errorf("force InstallHooks() count = %d, want 8", count)
	}
}

// TestInstallHooks_DoesNotAddDenyRule pins the retirement: a fresh install must
// leave no metadata deny rule behind. See agent.MetadataDenyRule for why.
func TestInstallHooks_DoesNotAddDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &FactoryAIDroidAgent{}
	_, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readFactoryPermissions(t, tempDir)
	if slices.Contains(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want no metadata deny rule", perms.Deny)
	}
}

// TestInstallHooks_RemovesStaleDenyRule is the migration: a config written by an
// older CLI still carries the rule, and a plain `entire enable` (no --force) has
// to drop it. This is the only thing that heals an existing repo on reinstall.
func TestInstallHooks_RemovesStaleDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeFactorySettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["`+agentpkg.MetadataDenyRule+`"]
  }
}`)

	a := &FactoryAIDroidAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readFactoryPermissions(t, tempDir)
	if slices.Contains(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want the stale rule removed", perms.Deny)
	}

	// And a second install stays clean rather than re-adding it.
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	perms = readFactoryPermissions(t, tempDir)
	if slices.Contains(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want it to stay removed", perms.Deny)
	}
}

// TestInstallHooks_RemovesOnlyOurDenyRule is the safety half of the migration:
// removal is keyed on the exact rule string Entire wrote, so a user's own deny
// rules survive untouched.
func TestInstallHooks_RemovesOnlyOurDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeFactorySettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["Bash(rm -rf *)", "`+agentpkg.MetadataDenyRule+`", "Read(./.env)"]
  }
}`)

	a := &FactoryAIDroidAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readFactoryPermissions(t, tempDir)
	for _, want := range []string{"Bash(rm -rf *)", "Read(./.env)"} {
		if !slices.Contains(perms.Deny, want) {
			t.Errorf("permissions.deny = %v, want to still contain user rule %q", perms.Deny, want)
		}
	}
	if slices.Contains(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want the Entire rule removed", perms.Deny)
	}
}

func TestInstallHooks_PermissionsDeny_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings.json with unknown permission fields like "ask"
	writeFactorySettingsFile(t, tempDir, `{
  "permissions": {
    "allow": ["Read(**)"],
    "ask": ["Write(**)", "Bash(*)"],
    "customField": {"nested": "value"}
  }
}`)

	agent := &FactoryAIDroidAgent{}
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read raw settings to check for unknown fields
	settingsPath := filepath.Join(tempDir, ".factory", "settings.json")
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

	// The deny rule is no longer added, and an empty deny array is dropped
	// rather than left behind.
	if denyRaw, ok := rawPermissions["deny"]; ok {
		var denyRules []string
		if err := json.Unmarshal(denyRaw, &denyRules); err != nil {
			t.Fatalf("failed to parse permissions.deny: %v", err)
		}
		if slices.Contains(denyRules, agentpkg.MetadataDenyRule) {
			t.Errorf("permissions.deny = %v, want no metadata deny rule", denyRules)
		}
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

//nolint:tparallel // Parent uses t.Chdir() which prevents t.Parallel(); subtests only read from pre-loaded data
func TestInstallHooks_PreservesUserHooksOnSameType(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with user hooks on the same hook types we use
	writeFactorySettingsFile(t, tempDir, `{
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

	agent := &FactoryAIDroidAgent{}
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	rawHooks := testutil.ReadRawHooks(t, tempDir, ".factory")

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		var matchers []FactoryHookMatcher
		if err := json.Unmarshal(rawHooks["Stop"], &matchers); err != nil {
			t.Fatalf("failed to parse Stop hooks: %v", err)
		}
		assertFactoryHookExists(t, matchers, "", "echo user stop hook", "user Stop hook")
		assertFactoryHookExists(t, matchers, "", agentpkg.WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", agentpkg.WarningFormatSingleLine), "Entire Stop hook")
	})

	t.Run("SessionStart", func(t *testing.T) {
		t.Parallel()
		var matchers []FactoryHookMatcher
		if err := json.Unmarshal(rawHooks["SessionStart"], &matchers); err != nil {
			t.Fatalf("failed to parse SessionStart hooks: %v", err)
		}
		assertFactoryHookExists(t, matchers, "", "echo user session start", "user SessionStart hook")
		assertFactoryHookExists(t, matchers, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-start"), "Entire SessionStart hook")
		assertFactoryHookExists(t, matchers, "", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid user-prompt-submit"), "Entire SessionStart user-prompt-submit hook")
	})

	t.Run("PostToolUse", func(t *testing.T) {
		t.Parallel()
		var matchers []FactoryHookMatcher
		if err := json.Unmarshal(rawHooks["PostToolUse"], &matchers); err != nil {
			t.Fatalf("failed to parse PostToolUse hooks: %v", err)
		}
		assertFactoryHookExists(t, matchers, "Write", "echo user wrote file", "user Write hook")
		assertFactoryHookExists(t, matchers, "Task", agentpkg.WrapProductionSilentHookCommand("entire hooks factoryai-droid post-tool-use"), "Entire Task hook")
	})
}

func TestInstallHooks_PreservesUnknownHookTypes(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with a hook type we don't handle (Notification is a hypothetical future hook type)
	writeFactorySettingsFile(t, tempDir, `{
  "hooks": {
    "Notification": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo notification received"}]
      }
    ],
    "SubagentStop": [
      {
        "matcher": ".*",
        "hooks": [{"type": "command", "command": "echo subagent stopped"}]
      }
    ]
  }
}`)

	agent := &FactoryAIDroidAgent{}
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read raw settings to check for unknown hook types
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".factory")

	// Verify Notification hook is preserved
	if _, ok := rawHooks["Notification"]; !ok {
		t.Errorf("Notification hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify SubagentStop hook is preserved
	if _, ok := rawHooks["SubagentStop"]; !ok {
		t.Errorf("SubagentStop hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify the Notification hook content is intact
	var notificationMatchers []FactoryHookMatcher
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

	// Verify the SubagentStop hook content is intact
	var subagentStopMatchers []FactoryHookMatcher
	if err := json.Unmarshal(rawHooks["SubagentStop"], &subagentStopMatchers); err != nil {
		t.Fatalf("failed to parse SubagentStop hooks: %v", err)
	}
	if len(subagentStopMatchers) != 1 {
		t.Errorf("SubagentStop matchers = %d, want 1", len(subagentStopMatchers))
	}
	if len(subagentStopMatchers) > 0 {
		if subagentStopMatchers[0].Matcher != ".*" {
			t.Errorf("SubagentStop matcher = %q, want %q", subagentStopMatchers[0].Matcher, ".*")
		}
		if len(subagentStopMatchers[0].Hooks) > 0 {
			if subagentStopMatchers[0].Hooks[0].Command != "echo subagent stopped" {
				t.Errorf("SubagentStop hook command = %q, want %q",
					subagentStopMatchers[0].Hooks[0].Command, "echo subagent stopped")
			}
		}
	}

	// Verify our hooks were also installed
	if _, ok := rawHooks["Stop"]; !ok {
		t.Errorf("Stop hook should have been installed")
	}
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &FactoryAIDroidAgent{}

	// First install
	_, err := agent.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify hooks are installed
	if !hooksInstalledNow(t, agent) {
		t.Error("hooks should be installed before uninstall")
	}

	// Uninstall
	err = agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify hooks are removed
	if hooksInstalledNow(t, agent) {
		t.Error("hooks should not be installed after uninstall")
	}
}

func TestUninstallHooks_NoSettingsFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	agent := &FactoryAIDroidAgent{}

	// Should not error when no settings file exists
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() should not error when no settings file: %v", err)
	}
}

// TestUninstallHooks_UnreadableSettingsErrors pins the absent-vs-unreadable
// split: an absent settings file means nothing to uninstall, but a read error
// must surface instead of reporting success with hooks still on disk. The
// settings path is created as a directory so os.ReadFile fails with a
// non-ErrNotExist error on every platform.
func TestUninstallHooks_UnreadableSettingsErrors(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.MkdirAll(filepath.Join(tempDir, ".factory", FactorySettingsFileName), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if err := (&FactoryAIDroidAgent{}).UninstallHooks(context.Background()); err == nil {
		t.Fatal("UninstallHooks() = nil for unreadable settings, want error")
	}
}

func TestUninstallHooks_PreservesUserHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with both user and entire hooks
	writeFactorySettingsFile(t, tempDir, `{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo user hook"}]
      },
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "entire hooks factoryai-droid stop"}]
      }
    ]
  }
}`)

	agent := &FactoryAIDroidAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	settings := readFactorySettings(t, tempDir)

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

	// Install no longer adds the rule, so stand in for a config written by an
	// older CLI: uninstall still has to clean it up.
	writeFactorySettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["`+agentpkg.MetadataDenyRule+`"]
  }
}`)

	agent := &FactoryAIDroidAgent{}

	if err := agent.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	perms := readFactoryPermissions(t, tempDir)
	if slices.Contains(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Error("deny rule should be removed after uninstall")
	}
}

func TestUninstallHooks_PreservesUserDenyRules(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with user deny rule and entire deny rule
	writeFactorySettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["Bash(rm -rf *)", "Read(./.entire/metadata/**)"]
  },
  "hooks": {
    "Stop": [
      {
        "hooks": [{"type": "command", "command": "entire hooks factoryai-droid stop"}]
      }
    ]
  }
}`)

	agent := &FactoryAIDroidAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	perms := readFactoryPermissions(t, tempDir)

	// Verify user deny rule is preserved
	if !slices.Contains(perms.Deny, "Bash(rm -rf *)") {
		t.Errorf("user deny rule was removed, got: %v", perms.Deny)
	}

	// Verify entire deny rule is removed
	if slices.Contains(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("entire deny rule should be removed, got: %v", perms.Deny)
	}
}

func TestUninstallHooks_PreservesUnknownHookTypes(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create settings with Entire hooks AND unknown hook types
	writeFactorySettingsFile(t, tempDir, `{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "entire hooks factoryai-droid stop"}]
      }
    ],
    "Notification": [
      {
        "matcher": "",
        "hooks": [{"type": "command", "command": "echo notification received"}]
      }
    ],
    "SubagentStop": [
      {
        "matcher": ".*",
        "hooks": [{"type": "command", "command": "echo subagent stopped"}]
      }
    ]
  }
}`)

	agent := &FactoryAIDroidAgent{}
	err := agent.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Read raw settings to check for unknown hook types
	rawHooks := testutil.ReadRawHooks(t, tempDir, ".factory")

	// Verify Notification hook is preserved
	if _, ok := rawHooks["Notification"]; !ok {
		t.Errorf("Notification hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify SubagentStop hook is preserved
	if _, ok := rawHooks["SubagentStop"]; !ok {
		t.Errorf("SubagentStop hook type was not preserved, got keys: %v", testutil.GetKeys(rawHooks))
	}

	// Verify our hooks were removed
	if _, ok := rawHooks["Stop"]; ok {
		// Check if there are any hooks left (should be empty after uninstall)
		var stopMatchers []FactoryHookMatcher
		if err := json.Unmarshal(rawHooks["Stop"], &stopMatchers); err == nil && len(stopMatchers) > 0 {
			t.Errorf("Stop hook should have been removed")
		}
	}
}

// Helper functions

// testPermissions is used only for test assertions
type testPermissions struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

func writeFactorySettingsFile(t *testing.T, tempDir, content string) {
	t.Helper()
	factoryDir := filepath.Join(tempDir, ".factory")
	if err := os.MkdirAll(factoryDir, 0o755); err != nil {
		t.Fatalf("failed to create .factory dir: %v", err)
	}
	settingsPath := filepath.Join(factoryDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}
}

func readFactoryPermissions(t *testing.T, tempDir string) testPermissions {
	t.Helper()
	settingsPath := filepath.Join(tempDir, ".factory", "settings.json")
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

func readFactorySettings(t *testing.T, tempDir string) FactorySettings {
	t.Helper()
	settingsPath := filepath.Join(tempDir, ".factory", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var settings FactorySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	return settings
}

func assertFactoryHookExists(t *testing.T, matchers []FactoryHookMatcher, matcher, command, description string) {
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

// hooksInstalledNow reports whether the agent's hooks are installed, failing the
// test if it could not tell. Built-in agents read a local config file where
// absent means absent, so an error here is a bug, not a state to tolerate.
func hooksInstalledNow(t *testing.T, ag interface {
	AreHooksInstalled(ctx context.Context) (bool, error)
},
) bool {
	t.Helper()

	installed, err := ag.AreHooksInstalled(context.Background())
	if err != nil {
		t.Fatalf("AreHooksInstalled() error = %v", err)
	}
	return installed
}
