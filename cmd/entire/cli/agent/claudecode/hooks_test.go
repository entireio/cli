package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"

	"github.com/stretchr/testify/require"
)

// TestInstallHooks_DoesNotAddDenyRule pins the retirement: a fresh install must
// leave no metadata deny rule behind. See agent.MetadataDenyRule for why.
func TestInstallHooks_DoesNotAddDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)
	if containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want no metadata deny rule", perms.Deny)
	}
}

// TestInstallHooks_RemovesStaleDenyRule is the migration that heals an existing
// repo: a config written by an older CLI still carries the rule, and a plain
// `entire enable` (no --force) has to drop it. Removal is what makes the write
// happen at all, so the second install must also leave it gone rather than
// re-adding it.
func TestInstallHooks_RemovesStaleDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["`+agentpkg.MetadataDenyRule+`"]
  }
}`)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if perms := readPermissions(t, tempDir); containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Fatalf("permissions.deny = %v, want the stale rule removed", perms.Deny)
	}

	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if perms := readPermissions(t, tempDir); containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want it to stay removed", perms.Deny)
	}
}

// TestInstallHooks_RemovesOnlyOurDenyRule is the safety half of the migration:
// removal is keyed on the exact rule string Entire wrote, so a user's own deny
// rules survive untouched.
func TestInstallHooks_RemovesOnlyOurDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["Bash(rm -rf *)", "`+agentpkg.MetadataDenyRule+`", "Read(./.env)"]
  }
}`)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)
	for _, want := range []string{"Bash(rm -rf *)", "Read(./.env)"} {
		if !containsRule(perms.Deny, want) {
			t.Errorf("permissions.deny = %v, want to still contain user rule %q", perms.Deny, want)
		}
	}
	if containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want the Entire rule removed", perms.Deny)
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

// TestInstallHooks_DropsEmptyDenyArray covers the config Entire fully owned: with
// its only rule removed, `deny` is deleted rather than left as an empty array.
func TestInstallHooks_DropsEmptyDenyArray(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["`+agentpkg.MetadataDenyRule+`"]
  }
}`)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// nil means the key is gone; "deny": [] would unmarshal to a non-nil empty
	// slice. An absent permissions block leaves the struct zero-valued, which is
	// also correct here.
	if perms := readPermissions(t, tempDir); perms.Deny != nil {
		t.Errorf("permissions.deny should be dropped once empty, got %v", perms.Deny)
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

	// The deny rule is no longer added. The seed carried no `deny` key, so the
	// key staying absent is the expected outcome — install must not create one
	// just to hold a rule it no longer writes.
	if denyRaw, present := rawPermissions["deny"]; present {
		var denyRules []string
		if err := json.Unmarshal(denyRaw, &denyRules); err != nil {
			t.Fatalf("failed to parse permissions.deny: %v", err)
		}
		if containsRule(denyRules, agentpkg.MetadataDenyRule) {
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
	if !hooksInstalledNow(t, agent) {
		t.Error("hooks should be installed before uninstall")
	}
	settings := readClaudeSettings(t, tempDir)
	if !hasEntireHook(settings.Hooks.SubagentStop) {
		t.Fatal("SubagentStop hook should be installed before uninstall")
	}
	if !hasEntireHook(settings.Hooks.StopFailure) {
		t.Fatal("StopFailure hook should be installed before uninstall")
	}
	if !hasEntireHook(settings.Hooks.SubagentStart) {
		t.Fatal("SubagentStart hook should be installed before uninstall")
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

	// RemovesSubagentStop: `entire disable` must strip the SubagentStop hook
	// installed by InstallHooks — without threading it through UninstallHooks,
	// disabling Entire would leave this hook behind.
	t.Run("removes SubagentStop", func(t *testing.T) {
		settings := readClaudeSettings(t, tempDir)
		if hasEntireHook(settings.Hooks.SubagentStop) {
			t.Error("SubagentStop hook should be removed after uninstall")
		}
	})

	// Uninstall must thread SubagentStart through too, or `entire disable`
	// leaves it behind.
	t.Run("removes SubagentStart", func(t *testing.T) {
		settings := readClaudeSettings(t, tempDir)
		if hasEntireHook(settings.Hooks.SubagentStart) {
			t.Error("SubagentStart hook should be removed after uninstall")
		}
	})

	// AreHooksInstalled only looks at Stop, so StopFailure needs its own check:
	// a stop-failure hook left behind would keep running after `entire disable`.
	t.Run("removes StopFailure", func(t *testing.T) {
		settings := readClaudeSettings(t, tempDir)
		if hasEntireHook(settings.Hooks.StopFailure) {
			t.Error("StopFailure hook should be removed after uninstall")
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

// TestUninstallHooks_UnreadableSettingsErrors pins the absent-vs-unreadable
// split: an absent settings file means nothing to uninstall, but a read error
// must surface instead of reporting success with hooks still on disk. The
// settings path is created as a directory so os.ReadFile fails with a
// non-ErrNotExist error on every platform.
func TestUninstallHooks_UnreadableSettingsErrors(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.MkdirAll(filepath.Join(tempDir, ".claude", ClaudeSettingsFileName), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if err := (&ClaudeCodeAgent{}).UninstallHooks(context.Background()); err == nil {
		t.Fatal("UninstallHooks() = nil for unreadable settings, want error")
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

	// Install no longer adds the rule, so stand in for a config written by an
	// older CLI: uninstall still has to clean it up.
	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "deny": ["`+agentpkg.MetadataDenyRule+`"]
  }
}`)

	agent := &ClaudeCodeAgent{}
	if err := agent.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	perms := readPermissions(t, tempDir)
	if containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
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
	if containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
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
	if !hooksInstalledNow(t, ag) {
		t.Fatal("legacy local-dev hooks should be detected as installed")
	}
	if err := ag.UninstallHooks(ctx); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}
	if hooksInstalledNow(t, ag) {
		t.Fatal("legacy local-dev hooks should be removed after uninstall")
	}
}

// expectedHookTimeout is one installed Entire hook and the timeout it must carry.
type expectedHookTimeout struct {
	hookType string
	matcher  string
	command  string
	timeout  int
}

func expectedHookTimeouts() []expectedHookTimeout {
	silent := agentpkg.WrapProductionSilentHookCommand
	return []expectedHookTimeout{
		{"SessionStart", "", agentpkg.WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", agentpkg.WarningFormatMultiLine), 30},
		{"UserPromptSubmit", "", silent("entire hooks claude-code user-prompt-submit"), 30},
		{"SubagentStart", workflowAgentMatcher, silent("entire hooks claude-code subagent-start"), 30},
		{"PreToolUse", subagentToolMatcher, silent("entire hooks claude-code pre-task"), 30},
		{"Stop", "", silent("entire hooks claude-code stop"), 120},
		{"StopFailure", "", silent("entire hooks claude-code stop-failure"), 120},
		{"SubagentStop", "", silent("entire hooks claude-code subagent-stop"), 120},
		{"PostToolUse", subagentToolMatcher, silent("entire hooks claude-code post-task"), 120},
		{"PostToolUse", skillToolMatcher, silent("entire hooks claude-code post-task"), 120},
		// Claude Code gives SessionEnd a short exit grace by default; an explicit
		// timeout would make it wait longer on exit, not shorter.
		{"SessionEnd", "", silent("entire hooks claude-code session-end"), 0},
	}
}

func hookMatchersByType(settings ClaudeSettings) map[string][]ClaudeHookMatcher {
	h := settings.Hooks
	return map[string][]ClaudeHookMatcher{
		"SessionStart":     h.SessionStart,
		"SessionEnd":       h.SessionEnd,
		"UserPromptSubmit": h.UserPromptSubmit,
		"SubagentStart":    h.SubagentStart,
		"SubagentStop":     h.SubagentStop,
		"Stop":             h.Stop,
		"StopFailure":      h.StopFailure,
		"PreToolUse":       h.PreToolUse,
		"PostToolUse":      h.PostToolUse,
	}
}

func assertHookTimeouts(t *testing.T, settings ClaudeSettings) {
	t.Helper()
	byType := hookMatchersByType(settings)
	for _, want := range expectedHookTimeouts() {
		hook, ok := lookupHook(byType[want.hookType], want.matcher, want.command)
		if !ok {
			t.Errorf("%s hook not found (matcher=%q, command=%q)", want.hookType, want.matcher, want.command)
			continue
		}
		if hook.Timeout != want.timeout {
			t.Errorf("%s (matcher=%q) timeout = %d, want %d", want.hookType, want.matcher, hook.Timeout, want.timeout)
		}
	}
}

// TestInstallHooks_SetsHookTimeouts pins a per-hook timeout on every Entire hook
// that Claude Code waits on, so a stalled `entire` launch cannot block startup,
// prompts, or turn ends indefinitely.
func TestInstallHooks_SetsHookTimeouts(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	assertHookTimeouts(t, settings)

	// SessionEnd must omit the field entirely (omitempty), not write "timeout": 0.
	raw, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	var parsed struct {
		Hooks map[string][]struct {
			Hooks []map[string]json.RawMessage `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	for _, m := range parsed.Hooks["SessionEnd"] {
		for _, h := range m.Hooks {
			if _, ok := h["timeout"]; ok {
				t.Errorf("SessionEnd hook must not carry a timeout field, got:\n%s", raw)
			}
		}
	}
}

// TestInstallHooks_BackfillsMissingTimeouts covers configs written by older CLIs:
// the hook is already present without a timeout, so nothing is added, but the
// install must still write the timeout.
func TestInstallHooks_BackfillsMissingTimeouts(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeClaudeStopHooks(t, tempDir, []string{agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")})

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	assertHookTimeouts(t, readClaudeSettings(t, tempDir))
}

// TestInstallHooks_KeepsUserSetTimeout verifies an install never overrides a
// timeout the user chose for an Entire hook.
func TestInstallHooks_KeepsUserSetTimeout(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stopCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q, "timeout": 300}]}]
  }
}`, stopCmd))

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	hook, ok := lookupHook(readClaudeSettings(t, tempDir).Hooks.Stop, "", stopCmd)
	if !ok {
		t.Fatal("Stop hook not found")
	}
	if hook.Timeout != 300 {
		t.Errorf("Stop timeout = %d, want the user's 300", hook.Timeout)
	}
}

// TestInstallHooks_TimeoutsAreIdempotent verifies a second install of an
// up-to-date config neither adds hooks nor rewrites the file.
func TestInstallHooks_TimeoutsAreIdempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	settingsPath := filepath.Join(tempDir, ".claude", "settings.json")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(settingsPath, past, past); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	count, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Errorf("second InstallHooks() count = %d, want 0", count)
	}
	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.ModTime().Equal(past) {
		t.Errorf("settings.json was rewritten by an install that had nothing to change")
	}
}

// userStopMatcherJSON is a user-owned Stop matcher carrying fields Entire does
// not model: a prompt hook, an async command hook, and a matcher-level key.
const userStopMatcherJSON = `{
  "matcher": "",
  "comment": "team notifications",
  "hooks": [
    {"type": "prompt", "prompt": "Check the task is done", "timeout": 30},
    {"type": "command", "command": "./notify.sh > /dev/null && echo ok", "async": true, "statusMessage": "notifying"}
  ]
}`

// writeSettingsWithUserStopMatcher seeds an Entire Stop hook without a timeout
// (as older CLIs wrote it) next to userStopMatcherJSON.
func writeSettingsWithUserStopMatcher(t *testing.T, dir string) {
	t.Helper()
	stopCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	writeSettingsFile(t, dir, fmt.Sprintf(`{
  "hooks": {
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": %q}]},
      %s
    ]
  }
}`, stopCmd, userStopMatcherJSON))
}

// assertUserStopMatcherIntact fails unless the user's Stop matcher survives
// byte-for-byte in meaning: every field kept and none added.
func assertUserStopMatcherIntact(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	var parsed struct {
		Hooks struct {
			Stop []map[string]any `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(userStopMatcherJSON), &want); err != nil {
		t.Fatal(err)
	}
	for _, m := range parsed.Hooks.Stop {
		if m["comment"] != nil {
			require.Equal(t, want, m, "user Stop matcher changed")
			return
		}
	}
	t.Fatalf("user Stop matcher missing:\n%s", raw)
}

// TestInstallHooks_PreservesUserHookFields covers the upgrade rewrite: a
// timeout backfill must not strip fields from the user's own hooks.
func TestInstallHooks_PreservesUserHookFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeSettingsWithUserStopMatcher(t, tempDir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	assertUserStopMatcherIntact(t, tempDir)

	raw, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `\u003e`) || strings.Contains(string(raw), `\u0026`) {
		t.Errorf("settings.json must not HTML-escape commands, got:\n%s", raw)
	}
}

func TestUninstallHooks_PreservesUserHookFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeSettingsWithUserStopMatcher(t, tempDir)

	ag := &ClaudeCodeAgent{}
	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	assertUserStopMatcherIntact(t, tempDir)
}

// TestInstallHooks_KeepsUserHooksEntireCannotModel covers user hooks whose
// shape Entire's types do not fully model. A rewrite must leave them exactly as
// written: encoding/json would otherwise read a differently-cased key into a
// modeled field and write it twice, and a modeled field holding another type
// would fail the decode and drop every unmodeled field.
func TestInstallHooks_KeepsUserHooksEntireCannotModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		matcher string
	}{
		{
			name:    "differently-cased keys",
			matcher: `{"matcher": "user", "Matcher": "x", "hooks": [{"type": "command", "command": "./a.sh", "Command": "./b.sh", "Timeout": 5}]}`,
		},
		{
			name:    "fractional timeout",
			matcher: `{"matcher": "user", "hooks": [{"type": "command", "command": "./notify.sh", "timeout": 1.5, "async": true, "statusMessage": "notifying"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rawHooks := map[string]json.RawMessage{"Stop": json.RawMessage(`[` + tt.matcher + `]`)}

			installHookEntries(rawHooks, false)

			var stop []map[string]any
			require.NoError(t, json.Unmarshal(rawHooks["Stop"], &stop))
			var want map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.matcher), &want))
			require.Contains(t, stop, want, "user Stop matcher changed:\n%s", rawHooks["Stop"])
		})
	}
}

// TestInstallHooks_KeepsUnmodeledTimeoutOnEntireHook: a timeout the user set on
// an Entire hook is theirs even when Entire cannot model it, either because it
// is fractional or because an explicit 0 reads the same as no timeout. Neither
// install nor doctor backfills one, enable does not report the hook as
// outdated, and a rewrite keeps the value as written.
func TestInstallHooks_KeepsUnmodeledTimeoutOnEntireHook(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout string
	}{
		{"fractional", "45.5"},
		{"explicit zero", "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Chdir(tempDir)

			stopCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
			entry := fmt.Sprintf(`{"type": "command", "command": %q, "timeout": %s}`, stopCmd, tt.timeout)
			writeSettingsFile(t, tempDir, fmt.Sprintf(`{"hooks": {"Stop": [{"matcher": "", "hooks": [%s]}]}}`, entry))

			c := &ClaudeCodeAgent{}
			if c.HasStaleManagedHooks(context.Background()) {
				t.Errorf("HasStaleManagedHooks() = true for an Entire hook with user-set timeout %s", tt.timeout)
			}
			changed, err := c.RepairHookTimeouts(context.Background())
			if err != nil {
				t.Fatalf("RepairHookTimeouts() error = %v", err)
			}
			if changed {
				t.Errorf("RepairHookTimeouts() changed = true for user-set timeout %s", tt.timeout)
			}
			if _, err := c.InstallHooks(context.Background(), false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}

			stop := readClaudeSettingsHooksOnly(t, tempDir, "Stop")
			var hooks []map[string]any
			for _, m := range stop {
				data, err := json.Marshal(m.Hooks)
				require.NoError(t, err)
				var entries []map[string]any
				require.NoError(t, json.Unmarshal(data, &entries))
				hooks = append(hooks, entries...)
			}
			var want map[string]any
			require.NoError(t, json.Unmarshal([]byte(entry), &want))
			require.Equal(t, []map[string]any{want}, hooks, "Entire Stop hook changed or duplicated")
		})
	}
}

// TestClaudeHookEntry_RedecodeDropsKeptBytes: encoding/json reuses slice
// elements when decoding into an existing value, so bytes kept from an entry
// Entire could not model must not outlive the entry that replaced it.
func TestClaudeHookEntry_RedecodeDropsKeptBytes(t *testing.T) {
	t.Parallel()

	matchers := []ClaudeHookMatcher{}
	require.NoError(t, json.Unmarshal([]byte(`[{"matcher":"","hooks":[{"type":"command","command":"a","timeout":1.5}]}]`), &matchers))
	require.NoError(t, json.Unmarshal([]byte(`[{"matcher":"","hooks":[{"type":"command","command":"b","timeout":30}]}]`), &matchers))

	got, err := json.Marshal(matchers)
	require.NoError(t, err)
	require.JSONEq(t, `[{"matcher":"","hooks":[{"type":"command","command":"b","timeout":30}]}]`, string(got))
}

// TestHasStaleManagedHooks_MissingTimeout lets enable report a timeout
// backfill instead of calling the rewritten config "already installed".
func TestHasStaleManagedHooks_MissingTimeout(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeClaudeStopHooks(t, tempDir, []string{agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")})

	c := &ClaudeCodeAgent{}
	if !c.HasStaleManagedHooks(context.Background()) {
		t.Fatal("HasStaleManagedHooks() = false for a hook missing its timeout, want true")
	}
	if _, err := c.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if c.HasStaleManagedHooks(context.Background()) {
		t.Error("HasStaleManagedHooks() = true after install, want false")
	}
}

// TestHasStaleManagedHooks_FreshInstall: hooks a first install adds are new,
// not outdated, so enable must not say it updated them.
func TestHasStaleManagedHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if (&ClaudeCodeAgent{}).HasStaleManagedHooks(context.Background()) {
		t.Error("HasStaleManagedHooks() = true with no settings file, want false")
	}
}

// TestRepairHookTimeouts_FillsOnlyMissingTimeouts covers the doctor repair:
// it may only give Entire hooks their missing timeout, so the user's matcher, a
// hook type that does not parse, and the permissions block all stay as written.
func TestRepairHookTimeouts_FillsOnlyMissingTimeouts(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stopCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {
    "SessionStart": {"not": "a matcher list"},
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": %q}]},
      %s
    ]
  }
}`, stopCmd, userStopMatcherJSON))

	c := &ClaudeCodeAgent{}
	changed, err := c.RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("RepairHookTimeouts() error = %v", err)
	}
	if !changed {
		t.Fatal("RepairHookTimeouts() changed = false, want true")
	}

	hook, ok := lookupHook(readClaudeSettingsHooksOnly(t, tempDir, "Stop"), "", stopCmd)
	if !ok {
		t.Fatal("Entire Stop hook missing after repair")
	}
	if hook.Timeout != checkpointHookTimeoutSec {
		t.Errorf("Stop timeout = %d, want %d", hook.Timeout, checkpointHookTimeoutSec)
	}
	assertUserStopMatcherIntact(t, tempDir)

	raw, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Permissions json.RawMessage            `json:"permissions"`
		Hooks       map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	if got := slices.Sorted(maps.Keys(parsed.Hooks)); !slices.Equal(got, []string{"SessionStart", "Stop"}) {
		t.Errorf("hook types = %v, want only [SessionStart Stop]", got)
	}
	require.JSONEq(t, `{"not": "a matcher list"}`, string(parsed.Hooks["SessionStart"]), "unparseable hook type changed")
	require.JSONEq(t, `{"allow": ["Bash(ls)"]}`, string(parsed.Permissions), "permissions block changed")

	changed, err = c.RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("second RepairHookTimeouts() error = %v", err)
	}
	if changed {
		t.Error("second RepairHookTimeouts() changed = true, want false")
	}
}

// TestRepairHookTimeouts_DropsEmptyPermissions: the repair writes through the
// same helper as install, so an empty permissions block goes the way install
// drops it rather than surviving only when doctor rewrote the file.
func TestRepairHookTimeouts_DropsEmptyPermissions(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stopCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "permissions": {},
  "hooks": {"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q}]}]}
}`, stopCmd))

	changed, err := (&ClaudeCodeAgent{}).RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("RepairHookTimeouts() error = %v", err)
	}
	if !changed {
		t.Fatal("RepairHookTimeouts() changed = false, want true")
	}
	raw, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &parsed))
	if _, ok := parsed["permissions"]; ok {
		t.Errorf("empty permissions block kept after repair:\n%s", raw)
	}
}

// TestRepairHookTimeouts_NoSettingsFile: doctor runs where Claude Code was
// never set up, and the repair must not create a config there.
func TestRepairHookTimeouts_NoSettingsFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	changed, err := (&ClaudeCodeAgent{}).RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("RepairHookTimeouts() error = %v", err)
	}
	if changed {
		t.Error("RepairHookTimeouts() changed = true with no settings file")
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".claude")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf(".claude exists after repair with no settings file (stat err = %v)", err)
	}
}

// TestRepairHookTimeouts_KeepsUserSetTimeout: a timeout the user chose for an
// Entire hook is not missing, so the repair leaves the file untouched.
func TestRepairHookTimeouts_KeepsUserSetTimeout(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stopCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	content := fmt.Sprintf(`{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q, "timeout": 300}]}]
  }
}`, stopCmd)
	writeSettingsFile(t, tempDir, content)

	changed, err := (&ClaudeCodeAgent{}).RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("RepairHookTimeouts() error = %v", err)
	}
	if changed {
		t.Error("RepairHookTimeouts() changed = true for a user-set timeout")
	}
	raw, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != content {
		t.Errorf("settings.json changed:\n%s", raw)
	}
}

// readClaudeSettingsHooksOnly parses one hook type, for files where another
// hook type is deliberately malformed and readClaudeSettings would fail.
func readClaudeSettingsHooksOnly(t *testing.T, dir, hookType string) []ClaudeHookMatcher {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	var parsed struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	var matchers []ClaudeHookMatcher
	if err := json.Unmarshal(parsed.Hooks[hookType], &matchers); err != nil {
		t.Fatalf("failed to parse %s: %v", hookType, err)
	}
	return matchers
}

// TestCaptureHookTimeoutExceedsStatusWalkBudget keeps Claude Code from killing
// a capture hook before its status walk can give up and degrade on its own.
func TestCaptureHookTimeoutExceedsStatusWalkBudget(t *testing.T) {
	t.Parallel()
	if time.Duration(captureHookTimeoutSec)*time.Second <= gitrepo.StatusWalkBudget {
		t.Errorf("captureHookTimeoutSec = %ds, want more than gitrepo.StatusWalkBudget (%s)", captureHookTimeoutSec, gitrepo.StatusWalkBudget)
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
		assertHookExists(t, matchers, "Skill", agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task"), "Entire Skill (forked skill) hook")
		assertNoEntireHookCommand(t, matchers, agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo"))
	})
}

// assertNoEntireHookCommand fails if any matcher still carries command.
func assertNoEntireHookCommand(t *testing.T, matchers []ClaudeHookMatcher, command string) {
	t.Helper()
	for _, m := range matchers {
		for _, h := range m.Hooks {
			if h.Command == command {
				t.Errorf("hook %q should not be installed (matcher %q)", command, m.Matcher)
			}
		}
	}
}

// TestInstallHooks_PrunesPostTodoHook pins that a plain install (no --force)
// removes the post-todo hook older CLIs installed: its handler records nothing,
// and a user's own hook under the same matcher survives.
func TestInstallHooks_PrunesPostTodoHook(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	todo := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "PostToolUse": [
      {"matcher": "TaskCreate|TaskUpdate", "hooks": [
        {"type": "command", "command": %q},
        {"type": "command", "command": "echo user task hook"}
      ]}
    ]
  }
}`, todo))

	c := &ClaudeCodeAgent{}
	if !c.HasStaleManagedHooks(context.Background()) {
		t.Fatal("HasStaleManagedHooks() = false before install, want true: enable must be able to report the prune")
	}
	if _, err := c.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if c.HasStaleManagedHooks(context.Background()) {
		t.Error("HasStaleManagedHooks() = true after install, want false")
	}

	settings := readClaudeSettings(t, tempDir)
	assertNoEntireHookCommand(t, settings.Hooks.PostToolUse, todo)
	assertHookExists(t, settings.Hooks.PostToolUse, "TaskCreate|TaskUpdate", "echo user task hook", "user hook under the old matcher")
	if got := CheckHookConfig(context.Background()); got != HooksCurrent {
		t.Errorf("CheckHookConfig() = %v, want HooksCurrent", got)
	}
}

// lookupHook returns the hook with the given matcher and command.
func lookupHook(matchers []ClaudeHookMatcher, matcher, command string) (ClaudeHookEntry, bool) {
	for _, m := range matchers {
		if m.Matcher != matcher {
			continue
		}
		for _, h := range m.Hooks {
			if h.Command == command {
				return h, true
			}
		}
	}
	return ClaudeHookEntry{}, false
}

// assertHookExists checks that a hook with the given matcher and command exists
func assertHookExists(t *testing.T, matchers []ClaudeHookMatcher, matcher, command, description string) {
	t.Helper()
	if _, ok := lookupHook(matchers, matcher, command); !ok {
		t.Errorf("%s was not found (matcher=%q, command=%q)", description, matcher, command)
	}
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
	// post-todo records nothing anymore, so it is not installed.
	assertNoEntireHookCommand(t, settings.Hooks.PostToolUse,
		agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-todo"))

	// SubagentStop fresh install is the regression for wiring up the real
	// background-subagent-completion signal (SubagentStop fires at true
	// completion, even for background subagents whose PostToolUse fires
	// seconds after launch at the launch stub). A fresh install must write the
	// SubagentStop hook with the same empty-matcher, availability-guarded
	// `sh -c` shape as Stop.
	t.Run("SubagentStop fresh install", func(t *testing.T) {
		wantCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-stop")
		assertHookExists(t, settings.Hooks.SubagentStop, "", wantCmd, "SubagentStop hook")

		// Same shape as Stop: single command, empty matcher, Stop's timeout.
		if len(settings.Hooks.SubagentStop) != 1 || settings.Hooks.SubagentStop[0].Matcher != "" {
			t.Fatalf("SubagentStop = %+v, want a single matcher with an empty string matcher (same shape as Stop)", settings.Hooks.SubagentStop)
		}
		if len(settings.Hooks.SubagentStop[0].Hooks) != 1 {
			t.Fatalf("SubagentStop hooks = %d, want 1", len(settings.Hooks.SubagentStop[0].Hooks))
		}
		if got, want := settings.Hooks.SubagentStop[0].Hooks[0].Timeout, settings.Hooks.Stop[0].Hooks[0].Timeout; got != want {
			t.Errorf("SubagentStop timeout = %d, want %d (same as Stop)", got, want)
		}
	})
}

// TestInstallHooks_SubagentStart_WorkflowMatcher pins the SubagentStart hook
// (#2685). Claude Code launches Workflow agents without an Agent tool call, so
// SubagentStart is the only launch signal Entire sees for them. The matcher
// filters on agent type, and "workflow-subagent" keeps direct Agent launches
// (which PreToolUse/PostToolUse[Agent] already record) from invoking Entire at
// all. See https://code.claude.com/docs/en/hooks.md (SubagentStart matcher).
func TestInstallHooks_SubagentStart_WorkflowMatcher(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	wantCmd := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-start")
	settings := readClaudeSettings(t, tempDir)
	if len(settings.Hooks.SubagentStart) != 1 || len(settings.Hooks.SubagentStart[0].Hooks) != 1 {
		t.Fatalf("SubagentStart = %+v, want exactly one Entire hook", settings.Hooks.SubagentStart)
	}
	assertHookExists(t, settings.Hooks.SubagentStart, "workflow-subagent", wantCmd, "SubagentStart workflow hook")

	count, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count)
	}

	if _, err := a.InstallHooks(context.Background(), true); err != nil {
		t.Fatalf("forced InstallHooks() error = %v", err)
	}
	settings = readClaudeSettings(t, tempDir)
	if len(settings.Hooks.SubagentStart) != 1 || len(settings.Hooks.SubagentStart[0].Hooks) != 1 {
		t.Fatalf("SubagentStart after --force = %+v, want exactly one Entire hook", settings.Hooks.SubagentStart)
	}
	assertHookExists(t, settings.Hooks.SubagentStart, "workflow-subagent", wantCmd, "SubagentStart workflow hook after --force")
}

// TestUninstallHooks_KeepsUserSubagentStartHook: uninstall strips Entire's
// SubagentStart hook and leaves the user's own under the same hook type.
func TestUninstallHooks_KeepsUserSubagentStartHook(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeSettingsFile(t, tempDir, `{
  "hooks": {
    "SubagentStart": [{"matcher": "Explore", "hooks": [{"type": "command", "command": "echo explore started"}]}]
  }
}`)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if !hasEntireHook(readClaudeSettings(t, tempDir).Hooks.SubagentStart) {
		t.Fatal("precondition: Entire's SubagentStart hook should be installed")
	}
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	settings := readClaudeSettings(t, tempDir)
	if hasEntireHook(settings.Hooks.SubagentStart) {
		t.Error("Entire's SubagentStart hook should be removed")
	}
	assertHookExists(t, settings.Hooks.SubagentStart, "Explore", "echo explore started", "user's SubagentStart hook")
}

// TestInstallHooks_SubagentStart_UpgradeInPlace: a config written before
// SubagentStart existed gains it on a plain `entire enable`, nothing else.
func TestInstallHooks_SubagentStart_UpgradeInPlace(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("initial InstallHooks() error = %v", err)
	}
	writeSettingsFile(t, tempDir, settingsWithoutHookType(t, tempDir, "SubagentStart"))

	count, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("upgrade InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Errorf("upgrade InstallHooks() count = %d, want exactly 1 (the SubagentStart entry)", count)
	}
	if got := CheckHookConfig(context.Background()); got != HooksCurrent {
		t.Errorf("CheckHookConfig() after upgrade = %v, want HooksCurrent", got)
	}
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

// TestCheckHookConfig_Outdated_MissingTurnHook pins CheckHookConfig's drift
// detection for hooks added after the first install: a config that predates
// SubagentStart, SubagentStop or StopFailure (everything else current) must read as outdated
// so `entire doctor`/`entire enable --force` picks it up, not silently stay
// HooksCurrent forever.
func TestCheckHookConfig_Outdated_MissingTurnHook(t *testing.T) {
	for _, hookType := range []string{"SubagentStart", "SubagentStop", "StopFailure"} {
		t.Run(hookType, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Chdir(tempDir)
			if _, err := (&ClaudeCodeAgent{}).InstallHooks(context.Background(), false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
			writeSettingsFile(t, tempDir, settingsWithoutHookType(t, tempDir, hookType))

			if got := CheckHookConfig(context.Background()); got != HooksOutdated {
				t.Errorf("CheckHookConfig() = %v, want HooksOutdated (missing %s)", got, hookType)
			}
		})
	}
}

// settingsWithoutHookType returns the installed settings.json with one hook
// type removed, as a config written before Entire installed it would read.
func settingsWithoutHookType(t *testing.T, tempDir, hookType string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(tempDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	if _, ok := raw["hooks"][hookType]; !ok {
		t.Fatalf("installed settings have no %s hook to remove", hookType)
	}
	delete(raw["hooks"], hookType)
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("failed to marshal settings.json: %v", err)
	}
	return string(out)
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
// TestCheckHookConfig_Outdated_MissingSkillHook: a config from before Entire
// recorded forked skills has every other hook, and still reports outdated so
// `entire status` and `entire doctor` prompt a reinstall.
func TestCheckHookConfig_Outdated_MissingSkillHook(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	a := &ClaudeCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	settingsPath := filepath.Join(tempDir, ".claude", ClaudeSettingsFileName)
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings, hooks map[string]json.RawMessage
	var postToolUse []ClaudeHookMatcher
	require.NoError(t, json.Unmarshal(data, &settings))
	require.NoError(t, json.Unmarshal(settings["hooks"], &hooks))
	require.NoError(t, json.Unmarshal(hooks["PostToolUse"], &postToolUse))
	postToolUse = slices.DeleteFunc(postToolUse, func(m ClaudeHookMatcher) bool { return m.Matcher == "Skill" })
	hooks["PostToolUse"], err = json.Marshal(postToolUse)
	require.NoError(t, err)
	settings["hooks"], err = json.Marshal(hooks)
	require.NoError(t, err)
	data, err = json.Marshal(settings)
	require.NoError(t, err)
	writeSettingsFile(t, tempDir, string(data))

	if got := CheckHookConfig(context.Background()); got != HooksOutdated {
		t.Errorf("CheckHookConfig() = %v, want HooksOutdated without the Skill hook", got)
	}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if got := CheckHookConfig(context.Background()); got != HooksCurrent {
		t.Errorf("CheckHookConfig() after reinstall = %v, want HooksCurrent", got)
	}
}

// TestCheckHookConfig_Outdated_WhenInstallWouldRewrite: a config that has every
// required hook but that an install would still rewrite is outdated, so
// `entire status` and `entire doctor` surface it instead of calling it current.
func TestCheckHookConfig_Outdated_WhenInstallWouldRewrite(t *testing.T) {
	stopCmd, err := jsonutil.MarshalWithNoHTMLEscape(agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop"))
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "missing timeouts",
			mutate: func(b []byte) []byte {
				return regexp.MustCompile(`,\s*"timeout": \d+`).ReplaceAll(b, nil)
			},
		},
		{
			// Written before the sh -c wrapper existed; a stalled launch blocks it too.
			name: "older command shape",
			mutate: func(b []byte) []byte {
				return bytes.Replace(b, stopCmd, []byte(`"entire hooks claude-code stop"`), 1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Chdir(tempDir)
			a := &ClaudeCodeAgent{}
			if _, err := a.InstallHooks(context.Background(), false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
			settingsPath := filepath.Join(tempDir, ".claude", ClaudeSettingsFileName)
			data, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			mutated := tt.mutate(data)
			require.NotEqual(t, data, mutated, "fixture mutation changed nothing")
			writeSettingsFile(t, tempDir, string(mutated))

			if got := CheckHookConfig(context.Background()); got != HooksOutdated {
				t.Errorf("CheckHookConfig() = %v, want HooksOutdated", got)
			}
			if _, err := a.InstallHooks(context.Background(), false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
			if got := CheckHookConfig(context.Background()); got != HooksCurrent {
				t.Errorf("CheckHookConfig() after reinstall = %v, want HooksCurrent", got)
			}
		})
	}
}

func TestCheckHookConfig_SupersetMatchersAreCurrent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	stop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	stopFailure := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code stop-failure")
	subagentStart := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-start")
	subagentStop := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code subagent-stop")
	pre := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code pre-task")
	post := agentpkg.WrapProductionSilentHookCommand("entire hooks claude-code post-task")
	// "Agent|Foo" still covers Agent; "Agent|Skill|Foo" still covers Agent and
	// Skill; "workflow-subagent|Explore" still covers workflow-subagent.
	writeSettingsFile(t, tempDir, fmt.Sprintf(`{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": %q, "timeout": %d}]}],
    "StopFailure": [{"matcher": "", "hooks": [{"type": "command", "command": %q, "timeout": %d}]}],
    "SubagentStart": [{"matcher": "workflow-subagent|Explore", "hooks": [{"type": "command", "command": %q, "timeout": %d}]}],
    "SubagentStop": [{"matcher": "", "hooks": [{"type": "command", "command": %q, "timeout": %d}]}],
    "PreToolUse": [{"matcher": "Agent|Foo", "hooks": [{"type": "command", "command": %q, "timeout": %d}]}],
    "PostToolUse": [{"matcher": "Agent|Skill|Foo", "hooks": [{"type": "command", "command": %q, "timeout": %d}]}]
  }
}`, stop, checkpointHookTimeoutSec, stopFailure, checkpointHookTimeoutSec, subagentStart, captureHookTimeoutSec,
		subagentStop, checkpointHookTimeoutSec, pre, captureHookTimeoutSec, post, checkpointHookTimeoutSec))

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
	assertNoEntireHookCommand(t, settings.Hooks.PostToolUse, staleTodo)
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

// TestPermissionConfigOwner_RepairsRetiredDenyRule exercises the shared repair
// path end to end through the real agent: the capability wiring, the read, the
// removal, and the write-back. This is what `entire doctor` calls, and it is a
// separate path from InstallHooks (a user who already enabled Entire has no
// reason to run enable again).
func TestPermissionConfigOwner_RepairsRetiredDenyRule(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeSettingsFile(t, tempDir, `{
  "permissions": {
    "allow": ["Read(**)"],
    "deny": ["Bash(rm -rf *)", "`+agentpkg.MetadataDenyRule+`"]
  }
}`)

	a := &ClaudeCodeAgent{}
	ctx := context.Background()

	if !agentpkg.HasRetiredMetadataDenyRule(ctx, a) {
		t.Fatal("HasRetiredMetadataDenyRule = false, want true before repair")
	}

	changed, err := agentpkg.RepairRetiredMetadataDenyRule(ctx, a)
	if err != nil {
		t.Fatalf("RepairRetiredMetadataDenyRule() error = %v", err)
	}
	if !changed {
		t.Error("RepairRetiredMetadataDenyRule() changed = false, want true")
	}

	perms := readPermissions(t, tempDir)
	if containsRule(perms.Deny, agentpkg.MetadataDenyRule) {
		t.Errorf("permissions.deny = %v, want the retired rule removed", perms.Deny)
	}
	if !containsRule(perms.Deny, "Bash(rm -rf *)") {
		t.Errorf("permissions.deny = %v, want the user rule preserved", perms.Deny)
	}
	if !containsRule(perms.Allow, "Read(**)") {
		t.Errorf("permissions.allow = %v, want it preserved", perms.Allow)
	}

	// Idempotent: doctor runs repeatedly, and the second run must report no
	// change rather than rewriting the file every time.
	if agentpkg.HasRetiredMetadataDenyRule(ctx, a) {
		t.Error("HasRetiredMetadataDenyRule = true after repair")
	}
	changed, err = agentpkg.RepairRetiredMetadataDenyRule(ctx, a)
	if err != nil {
		t.Fatalf("second RepairRetiredMetadataDenyRule() error = %v", err)
	}
	if changed {
		t.Error("second RepairRetiredMetadataDenyRule() changed = true, want false")
	}
}

// TestPermissionConfigOwner_NoConfigFile: doctor runs in repos where the agent
// was never set up. Absent must read as "nothing to report", not an error.
func TestPermissionConfigOwner_NoConfigFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	a := &ClaudeCodeAgent{}
	ctx := context.Background()

	if agentpkg.HasRetiredMetadataDenyRule(ctx, a) {
		t.Error("HasRetiredMetadataDenyRule = true with no settings file")
	}
	changed, err := agentpkg.RepairRetiredMetadataDenyRule(ctx, a)
	if err != nil {
		t.Errorf("RepairRetiredMetadataDenyRule() error = %v, want nil", err)
	}
	if changed {
		t.Error("RepairRetiredMetadataDenyRule() changed = true with no settings file")
	}
}

// TestPermissionConfigOwner_UnparseableConfig splits the two contracts: the
// detector stays quiet (diagnostics must not accuse on a failed read) while the
// repair reports, because in doctor an unparseable config is the answer.
func TestPermissionConfigOwner_UnparseableConfig(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	writeSettingsFile(t, tempDir, `{not json`)

	a := &ClaudeCodeAgent{}
	ctx := context.Background()

	if agentpkg.HasRetiredMetadataDenyRule(ctx, a) {
		t.Error("HasRetiredMetadataDenyRule = true on an unparseable config")
	}
	if _, err := agentpkg.RepairRetiredMetadataDenyRule(ctx, a); err == nil {
		t.Error("RepairRetiredMetadataDenyRule() error = nil, want a parse error")
	}
}
