package geminicli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGeminiHookSpecs_ExactInventory(t *testing.T) {
	t.Parallel()
	want := []string{
		"SessionStart/entire-session-start/session-start",
		"SessionEnd/entire-session-end-exit/session-end",
		"SessionEnd/entire-session-end-logout/session-end",
		"BeforeAgent/entire-before-agent/before-agent",
		"AfterAgent/entire-after-agent/after-agent",
		"BeforeModel/entire-before-model/before-model",
		"AfterModel/entire-after-model/after-model",
		"BeforeToolSelection/entire-before-tool-selection/before-tool-selection",
		"BeforeTool/entire-before-tool/before-tool",
		"AfterTool/entire-after-tool/after-tool",
		"PreCompress/entire-pre-compress/pre-compress",
		"Notification/entire-notification/notification",
	}
	got := make([]string, 0, len(geminiHookSpecs))
	for _, spec := range geminiHookSpecs {
		got = append(got, spec.section+"/"+spec.name+"/"+spec.hookName)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("gemini inventory = %q, want %q", got, want)
	}
}

func geminiUserSettings(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".gemini", GeminiSettingsFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	return path
}

func geminiRawSettings(t *testing.T, path string) map[string]json.RawMessage {
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

func TestUserHooks_Gemini_Lifecycle(t *testing.T) {
	path := geminiUserSettings(t)
	const custom = "my-own-session-start"
	initial := `{"theme":"GitHub","hooks":{"SessionStart":[{"hooks":[{"name":"mine","type":"command","command":"` + custom + `"}]}]}}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	result, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Installed != len(geminiHookSpecs) || result.Repaired {
		t.Fatalf("install result = %+v, want %d fresh hooks", result, len(geminiHookSpecs))
	}
	installed, err := agent.AreUserHooksInstalled(context.Background())
	if err != nil || !installed {
		t.Fatalf("AreUserHooksInstalled = %v, %v", installed, err)
	}
	raw := geminiRawSettings(t, path)
	if string(raw["theme"]) != `"GitHub"` || !strings.Contains(string(raw["hooks"]), custom) || !strings.Contains(string(raw["hooksConfig"]), "true") {
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
	raw = geminiRawSettings(t, path)
	if !strings.Contains(string(raw["hooks"]), custom) || strings.Contains(string(raw["hooks"]), "entire hooks gemini") {
		t.Fatalf("uninstall did not preserve only user hooks: %s", raw["hooks"])
	}
}
