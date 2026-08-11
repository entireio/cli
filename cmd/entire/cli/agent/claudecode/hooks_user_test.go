package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No t.Parallel in this file: every test redirects the user home via t.Setenv.

// setUserHome points os.UserHomeDir at a temp dir (HOME on Unix, USERPROFILE
// on Windows) so user-level installs never touch the developer's real
// ~/.claude/settings.json.
func setUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func userSettingsTestPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".claude", ClaudeSettingsFileName)
}

func writeUserClaudeSettings(t *testing.T, home, content string) {
	t.Helper()
	path := userSettingsTestPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readRawSettings(t *testing.T, path string) map[string]json.RawMessage {
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

// collectHookCommands returns every hook command in the settings file at path.
func collectHookCommands(t *testing.T, path string) []string {
	t.Helper()
	raw := readRawSettings(t, path)
	hooksRaw, ok := raw["hooks"]
	if !ok {
		return nil
	}
	var hooks map[string][]ClaudeHookMatcher
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	var cmds []string
	for _, matchers := range hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				cmds = append(cmds, h.Command)
			}
		}
	}
	return cmds
}

const existingUserSettings = `{
  "model": "opus",
  "env": {"FOO": "bar"},
  "permissions": {"deny": ["Read(secret.txt)"]},
  "hooks": {
    "Notification": [{"matcher": "", "hooks": [{"type": "command", "command": "custom-notify"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "my-own-stop"}]}]
  }
}`

func TestInstallUserHooks_MergesWithoutClobberingUnrelatedKeys(t *testing.T) {
	home := setUserHome(t)
	writeUserClaudeSettings(t, home, existingUserSettings)

	agent := &ClaudeCodeAgent{}
	count, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallUserHooks() installed nothing on a file without Entire hooks")
	}

	raw := readRawSettings(t, userSettingsTestPath(t, home))
	if got := string(raw["model"]); got != `"opus"` {
		t.Errorf("model clobbered: %s", got)
	}
	if got := string(raw["env"]); !strings.Contains(got, `"FOO"`) {
		t.Errorf("env clobbered: %s", got)
	}
	// User-level installs must never touch user permissions (the deny rule is
	// repo-scope only).
	if got := string(raw["permissions"]); got != `{"deny":["Read(secret.txt)"]}` && !strings.Contains(got, "secret.txt") {
		t.Errorf("permissions clobbered: %s", got)
	}
	if strings.Contains(string(raw["permissions"]), ".entire/metadata") {
		t.Errorf("repo-scoped deny rule leaked into user permissions: %s", raw["permissions"])
	}

	cmds := collectHookCommands(t, userSettingsTestPath(t, home))
	var foundCustomStop, foundNotify, foundEntireStop bool
	for _, c := range cmds {
		switch {
		case c == "my-own-stop":
			foundCustomStop = true
		case c == "custom-notify":
			foundNotify = true
		case isEntireHook(c) && strings.Contains(c, "entire hooks claude-code stop"):
			foundEntireStop = true
		}
		// The plain production form only — never a repo-local dev path.
		if strings.Contains(c, "CLAUDE_PROJECT_DIR") || strings.Contains(c, "entire-dev") {
			t.Errorf("user-level hook uses a repo-local command form: %s", c)
		}
	}
	if !foundCustomStop || !foundNotify {
		t.Errorf("user's own hooks were removed (customStop=%v notify=%v)", foundCustomStop, foundNotify)
	}
	if !foundEntireStop {
		t.Errorf("Entire stop hook missing from user settings, commands: %v", cmds)
	}
	if !agent.AreUserHooksInstalled(context.Background()) {
		t.Error("AreUserHooksInstalled() = false after install")
	}
}

func TestInstallUserHooks_Idempotent(t *testing.T) {
	home := setUserHome(t)

	agent := &ClaudeCodeAgent{}
	first, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("first InstallUserHooks() error = %v", err)
	}
	if first == 0 {
		t.Fatal("first install must report installed hooks")
	}
	before, err := os.ReadFile(userSettingsTestPath(t, home))
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
	after, err := os.ReadFile(userSettingsTestPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("re-install rewrote the user settings file")
	}
}

func TestUninstallUserHooks_RemovesOnlyOurs(t *testing.T) {
	home := setUserHome(t)
	writeUserClaudeSettings(t, home, existingUserSettings)

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}

	if agent.AreUserHooksInstalled(context.Background()) {
		t.Error("Entire hooks still installed after uninstall")
	}
	raw := readRawSettings(t, userSettingsTestPath(t, home))
	if got := string(raw["model"]); got != `"opus"` {
		t.Errorf("model clobbered by uninstall: %s", got)
	}
	if !strings.Contains(string(raw["permissions"]), "secret.txt") {
		t.Errorf("user permissions clobbered by uninstall: %s", raw["permissions"])
	}
	cmds := collectHookCommands(t, userSettingsTestPath(t, home))
	var foundCustomStop, foundNotify bool
	for _, c := range cmds {
		if isEntireHook(c) {
			t.Errorf("Entire hook left behind: %s", c)
		}
		if c == "my-own-stop" {
			foundCustomStop = true
		}
		if c == "custom-notify" {
			foundNotify = true
		}
	}
	if !foundCustomStop || !foundNotify {
		t.Errorf("uninstall removed the user's own hooks (customStop=%v notify=%v)", foundCustomStop, foundNotify)
	}
}

func TestUninstallUserHooks_MissingFileIsNoError(t *testing.T) {
	setUserHome(t)
	agent := &ClaudeCodeAgent{}
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() on missing file = %v", err)
	}
}

// TestInstallUserHooks_NeverWritesRepoFiles pins the core global-enable
// invariant: the user-level install writes only user-scope config, even when
// invoked from inside a directory that could take a repo-level install.
func TestInstallUserHooks_NeverWritesRepoFiles(t *testing.T) {
	home := setUserHome(t)
	work := t.TempDir()
	t.Chdir(work)

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", ClaudeSettingsFileName)); !os.IsNotExist(err) {
		t.Error("user-level install created a repo-level .claude/settings.json")
	}
	if _, err := os.Stat(userSettingsTestPath(t, home)); err != nil {
		t.Errorf("user-level settings file missing: %v", err)
	}
}

// TestInstallUserHooks_CommandsMatchRepoProductionForms pins the dedup
// contract: Claude Code deduplicates identical hook commands across settings
// scopes, so a repo with both a repo-level production install and the
// user-level install fires each hook exactly once. That only holds while the
// two installs write byte-identical command strings.
func TestInstallUserHooks_CommandsMatchRepoProductionForms(t *testing.T) {
	home := setUserHome(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if _, err := agent.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	userCmds := entireCommandSet(t, userSettingsTestPath(t, home))
	repoCmds := entireCommandSet(t, filepath.Join(repoDir, ".claude", ClaudeSettingsFileName))
	if len(userCmds) == 0 {
		t.Fatal("no Entire hook commands at user level")
	}
	for c := range userCmds {
		if !repoCmds[c] {
			t.Errorf("user-level command not byte-identical to a repo-level production command: %s", c)
		}
	}
	for c := range repoCmds {
		if !userCmds[c] {
			t.Errorf("repo-level production command missing at user level: %s", c)
		}
	}
}

func entireCommandSet(t *testing.T, path string) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	for _, c := range collectHookCommands(t, path) {
		if isEntireHook(c) {
			set[c] = true
		}
	}
	return set
}
