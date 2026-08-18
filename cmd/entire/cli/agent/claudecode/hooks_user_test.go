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

// setUserHome points os.UserHomeDir at a temp dir so user-level installs never
// touch the developer's real ~/.claude/settings.json.
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

func countOf(cmds []string, want string) int {
	n := 0
	for _, c := range cmds {
		if c == want {
			n++
		}
	}
	return n
}

func agentWrappedStopCmd() string {
	return buildClaudeHookCommands(false).stop
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

const existingUserSettings = `{
  "model": "opus",
  "env": {"FOO": "bar"},
  "permissions": {"deny": ["Read(secret.txt)"]},
  "hooks": {
    "Notification": [{"matcher": "", "hooks": [{"type": "command", "command": "custom-notify"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "my-own-stop"}]}]
  }
}`

// TestUserHooks_Lifecycle is the anchor walk: install into a populated user
// file (merge semantics, permissions untouched, production command forms),
// re-install (byte-identical no-op), duplicate-entry repair, then uninstall
// (only Entire's entries removed).
func TestUserHooks_Lifecycle(t *testing.T) {
	home := setUserHome(t)
	agent := &ClaudeCodeAgent{}
	// A missing file is not an uninstall error.
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() on missing file = %v", err)
	}
	writeUserClaudeSettings(t, home, existingUserSettings)
	path := userSettingsTestPath(t, home)

	// Install: merge, never clobber, never touch permissions, production forms.
	res, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if res.Installed == 0 || res.Repaired {
		t.Fatalf("fresh install must report installs and no repair, got %+v", res)
	}
	raw := readRawSettings(t, path)
	if got := string(raw["model"]); got != `"opus"` {
		t.Errorf("model clobbered: %s", got)
	}
	if got := string(raw["env"]); !strings.Contains(got, `"FOO"`) {
		t.Errorf("env clobbered: %s", got)
	}
	if got := string(raw["permissions"]); !strings.Contains(got, "secret.txt") || strings.Contains(got, ".entire/metadata") {
		t.Errorf("user permissions altered: %s", got)
	}
	cmds := collectHookCommands(t, path)
	var foundEntireStop bool
	for _, c := range cmds {
		if isEntireHook(c) && strings.Contains(c, "entire hooks claude-code stop") {
			foundEntireStop = true
		}
		if strings.Contains(c, "CLAUDE_PROJECT_DIR") || strings.Contains(c, "entire-dev") {
			t.Errorf("user-level hook uses a repo-local command form: %s", c)
		}
	}
	if !foundEntireStop {
		t.Errorf("Entire stop hook missing from user settings, commands: %v", cmds)
	}
	if countOf(cmds, "my-own-stop") != 1 || countOf(cmds, "custom-notify") != 1 {
		t.Errorf("user's own hooks were removed: %v", cmds)
	}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || !ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after install, want (true, nil)", ok, err)
	}

	// Re-install: byte-identical no-op.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("second InstallUserHooks() error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Installed != 0 || second.Repaired || string(before) != string(after) {
		t.Errorf("second install must be a byte-identical no-op, got %+v", second)
	}

	// Duplicate an entry: the rewrite is Repaired, never "already installed".
	raw = readRawSettings(t, path)
	var hooks map[string][]ClaudeHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	// Duplicate Entire's wrapped Stop entry specifically — the file also
	// carries the user's own Stop matcher, which must never be deduped.
	duplicated := false
	for i, m := range hooks["Stop"] {
		for _, h := range m.Hooks {
			if h.Command == agentWrappedStopCmd() {
				hooks["Stop"][i].Hooks = append(hooks["Stop"][i].Hooks, h)
				duplicated = true
			}
		}
		if duplicated {
			break
		}
	}
	if !duplicated {
		t.Fatal("install left no wrapped Stop entry to duplicate")
	}
	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		t.Fatal(err)
	}
	raw["hooks"] = hooksJSON
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if res.Installed != 0 || !res.Repaired {
		t.Errorf("deduplication must report Installed=0, Repaired=true, got %+v", res)
	}
	if got := countOf(collectHookCommands(t, path), agentWrappedStopCmd()); got != 1 {
		t.Errorf("Stop entry count after repair = %d, want 1", got)
	}

	// Uninstall: only Entire's entries go.
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after uninstall, want (false, nil)", ok, err)
	}
	raw = readRawSettings(t, path)
	if got := string(raw["model"]); got != `"opus"` {
		t.Errorf("model clobbered by uninstall: %s", got)
	}
	if !strings.Contains(string(raw["permissions"]), "secret.txt") {
		t.Errorf("user permissions clobbered by uninstall: %s", raw["permissions"])
	}
	cmds = collectHookCommands(t, path)
	for _, c := range cmds {
		if isEntireHook(c) {
			t.Errorf("Entire hook left behind: %s", c)
		}
	}
	if countOf(cmds, "my-own-stop") != 1 || countOf(cmds, "custom-notify") != 1 {
		t.Errorf("uninstall removed the user's own hooks: %v", cmds)
	}
}

// A user-scope install never parses the permissions section: a non-object
// value must neither fail the install nor be rewritten.
func TestInstallUserHooks_NonObjectPermissionsRoundTripsVerbatim(t *testing.T) {
	home := setUserHome(t)
	writeUserClaudeSettings(t, home, `{"permissions": "deny-all", "hooks": {}}`)

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	raw := readRawSettings(t, userSettingsTestPath(t, home))
	if got := string(raw["permissions"]); got != `"deny-all"` {
		t.Errorf("permissions not preserved verbatim: got %s", got)
	}
}

// The user-level install writes only user-scope config, even when invoked from
// a directory that could take a repo-level install.
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

// Claude Code dedups identical hook commands across settings scopes, so
// no-double-fire holds only while user- and repo-level installs write
// byte-identical command strings.
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
			t.Errorf("user-level command not byte-identical to a repo-level command: %s", c)
		}
	}
	for c := range repoCmds {
		if !userCmds[c] {
			t.Errorf("repo-level production command missing at user level: %s", c)
		}
	}
}

// Verb-scoped recognition: a user-authored hook that merely invokes the entire
// binary must never be claimed as ours (a bare `entire ` prefix once was, and
// UninstallUserHooks deleted it).
func TestUserAuthoredEntireCLIHookSurvivesInstallAndUninstall(t *testing.T) {
	home := setUserHome(t)
	const userCmd = "entire status --json > /tmp/entire-status.json"
	writeUserClaudeSettings(t, home, `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "`+userCmd+`"}]}],
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "`+userCmd+`"}]}]
  }
}`)

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if got := countOf(collectHookCommands(t, userSettingsTestPath(t, home)), userCmd); got != 2 {
		t.Fatalf("user-authored entire-CLI hook count after install = %d, want 2", got)
	}
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}
	cmds := collectHookCommands(t, userSettingsTestPath(t, home))
	if got := countOf(cmds, userCmd); got != 2 {
		t.Errorf("user-authored entire-CLI hook count after uninstall = %d, want 2", got)
	}
	for _, c := range cmds {
		if isEntireHook(c) {
			t.Errorf("Entire hook left behind after uninstall: %s", c)
		}
	}
}

// Incomplete or alternate-form installs must read as not-installed and be
// repaired in place: a bare-form Entire hook is replaced (not joined — both
// once fired on every Stop machine-wide), and a Stop-only file is completed.
func TestInstallUserHooks_RepairsIncompleteInstalls(t *testing.T) {
	t.Run("alternate command form is replaced", func(t *testing.T) {
		home := setUserHome(t)
		writeUserClaudeSettings(t, home, `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]}]
  }
}`)
		agent := &ClaudeCodeAgent{}
		res, err := agent.InstallUserHooks(context.Background())
		if err != nil {
			t.Fatalf("InstallUserHooks() error = %v", err)
		}
		if !res.Repaired {
			t.Error("replacing an alternate-form entry must be reported as a repair")
		}
		cmds := collectHookCommands(t, userSettingsTestPath(t, home))
		if countOf(cmds, "entire hooks claude-code stop") != 0 {
			t.Error("bare-form Entire hook still present after repair")
		}
		if got := countOf(cmds, agentWrappedStopCmd()); got != 1 {
			t.Errorf("wrapped Stop entry count after repair = %d, want 1", got)
		}
	})
}

// A managed hook section in an unexpected shape aborts install AND uninstall
// with an error naming the section, leaving the file byte-identical.
func TestInstallUserHooks_UnparseableHookSectionErrorsCleanly(t *testing.T) {
	home := setUserHome(t)
	original := `{
  "hooks": {
    "Stop": {"unexpected": "shape"}
  }
}`
	writeUserClaudeSettings(t, home, original)

	agent := &ClaudeCodeAgent{}
	_, err := agent.InstallUserHooks(context.Background())
	if err == nil {
		t.Fatal("InstallUserHooks() must error on an unparseable hook section")
	}
	if !strings.Contains(err.Error(), "Stop") || !strings.Contains(err.Error(), userSettingsTestPath(t, home)) {
		t.Errorf("install error must name the section and file, got: %v", err)
	}
	if err := agent.UninstallUserHooks(context.Background()); err == nil {
		t.Fatal("UninstallUserHooks() must error on an unparseable hook section")
	}
	data, readErr := os.ReadFile(userSettingsTestPath(t, home))
	if readErr != nil || string(data) != original {
		t.Errorf("refused write must leave the file byte-identical (err=%v):\n%s", readErr, data)
	}
}
