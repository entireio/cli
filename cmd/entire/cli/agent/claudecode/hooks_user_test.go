package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeHookSpecs_ExactInventory(t *testing.T) {
	t.Parallel()
	want := []claudeHookSpec{
		{section: "SessionStart", hookName: HookNameSessionStart, warnWrap: true},
		{section: "SessionEnd", hookName: HookNameSessionEnd},
		{section: "Stop", hookName: HookNameStop},
		{section: "SubagentStop", hookName: HookNameSubagentStop},
		{section: "UserPromptSubmit", hookName: HookNameUserPromptSubmit},
		{section: "PreToolUse", matcher: subagentToolMatcher, hookName: HookNamePreTask},
		{section: "PostToolUse", matcher: subagentToolMatcher, hookName: HookNamePostTask},
		{section: "PostToolUse", matcher: taskToolMatcher, hookName: HookNamePostTodo},
	}
	if !reflect.DeepEqual(claudeHookSpecs, want) {
		t.Fatalf("claudeHookSpecs = %#v, want %#v", claudeHookSpecs, want)
	}
}

func claudeUserSettings(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".claude", ClaudeSettingsFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	return path
}

func claudeRawSettings(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUserHooks_Lifecycle(t *testing.T) {
	path := claudeUserSettings(t)
	const custom = "entire status --json"
	initial := `{"model":"opus","permissions":"deny-all","hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + custom + `"}]}]}}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &ClaudeCodeAgent{}
	result, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Installed != entireClaudeHookCount || result.Repaired {
		t.Fatalf("install result = %+v, want %d fresh hooks", result, entireClaudeHookCount)
	}
	installed, err := agent.AreUserHooksInstalled(context.Background())
	if err != nil || !installed {
		t.Fatalf("AreUserHooksInstalled = %v, %v", installed, err)
	}
	raw := claudeRawSettings(t, path)
	if string(raw["model"]) != `"opus"` || string(raw["permissions"]) != `"deny-all"` || !strings.Contains(string(raw["hooks"]), custom) {
		t.Fatalf("install changed user-owned settings: %s", raw)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err = agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Installed != 0 || result.Repaired || string(before) != string(after) {
		t.Fatalf("idempotent install changed file: %+v", result)
	}

	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw = claudeRawSettings(t, path)
	if !strings.Contains(string(raw["hooks"]), custom) || strings.Contains(string(raw["hooks"]), "entire hooks claude-code") {
		t.Fatalf("uninstall did not preserve only user hooks: %s", raw["hooks"])
	}
}
