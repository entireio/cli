package geminicli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No t.Parallel in this file: every test redirects the user home via t.Setenv.

func setUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func userGeminiSettingsPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".gemini", GeminiSettingsFileName)
}

func readRawGeminiSettings(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return raw
}

func TestInstallUserHooks_Gemini_MergesWithoutClobberingUnrelatedKeys(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "theme": "GitHub",
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "my-hook", "type": "command", "command": "my-own-session-start"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	count, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallUserHooks() installed nothing on a file without Entire hooks")
	}

	raw := readRawGeminiSettings(t, path)
	if got := string(raw["theme"]); got != `"GitHub"` {
		t.Errorf("theme clobbered: %s", got)
	}
	if !strings.Contains(string(raw["hooksConfig"]), "true") {
		t.Errorf("hooksConfig.enabled not set: %s", raw["hooksConfig"])
	}
	content := string(raw["hooks"])
	if !strings.Contains(content, "my-own-session-start") {
		t.Error("user's own hook was removed")
	}
	if !strings.Contains(content, "entire hooks gemini session-start") {
		t.Error("Entire session-start hook missing")
	}
	if strings.Contains(content, "entire-dev") || strings.Contains(content, "git rev-parse") {
		t.Errorf("user-level hooks use a repo-local command form: %s", content)
	}
	if !agent.AreUserHooksInstalled(context.Background()) {
		t.Error("AreUserHooksInstalled() = false after install")
	}
}

func TestInstallUserHooks_Gemini_Idempotent(t *testing.T) {
	home := setUserHome(t)

	agent := &GeminiCLIAgent{}
	first, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("first InstallUserHooks() error = %v", err)
	}
	if first == 0 {
		t.Fatal("first install must report installed hooks")
	}
	before, err := os.ReadFile(userGeminiSettingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("second InstallUserHooks() error = %v", err)
	}
	if second != 0 {
		t.Errorf("second install must be a no-op, got count %d", second)
	}
	after, err := os.ReadFile(userGeminiSettingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("re-install rewrote the user settings file")
	}
}

func TestUninstallUserHooks_Gemini_RemovesOnlyOurs(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	existing := `{"theme":"GitHub","hooks":{"SessionStart":[{"hooks":[{"name":"my-hook","type":"command","command":"my-own-session-start"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}

	if agent.AreUserHooksInstalled(context.Background()) {
		t.Error("Entire hooks still installed after uninstall")
	}
	raw := readRawGeminiSettings(t, path)
	if got := string(raw["theme"]); got != `"GitHub"` {
		t.Errorf("theme clobbered by uninstall: %s", got)
	}
	content := string(raw["hooks"])
	if !strings.Contains(content, "my-own-session-start") {
		t.Error("uninstall removed the user's own hook")
	}
	if strings.Contains(content, "entire hooks gemini") {
		t.Errorf("Entire hooks left behind: %s", content)
	}
}

func TestUninstallUserHooks_Gemini_MissingFileIsNoError(t *testing.T) {
	setUserHome(t)
	agent := &GeminiCLIAgent{}
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() on missing file = %v", err)
	}
}

func TestInstallUserHooks_Gemini_NeverWritesRepoFiles(t *testing.T) {
	home := setUserHome(t)
	work := t.TempDir()
	t.Chdir(work)

	agent := &GeminiCLIAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".gemini", GeminiSettingsFileName)); !os.IsNotExist(err) {
		t.Error("user-level install created a repo-level .gemini/settings.json")
	}
	if _, err := os.Stat(userGeminiSettingsPath(t, home)); err != nil {
		t.Errorf("user-level settings file missing: %v", err)
	}
}
