package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// TestInstallUserHooks_NonObjectPermissionsRoundTripsVerbatim pins that a
// user-scope install never parses the permissions section: a value that is
// not an object (which a project-scope install would reject) must neither
// fail the install nor be rewritten — it round-trips byte-for-byte.
func TestInstallUserHooks_NonObjectPermissionsRoundTripsVerbatim(t *testing.T) {
	home := setUserHome(t)
	writeUserClaudeSettings(t, home, `{
  "permissions": "deny-all",
  "hooks": {}
}`)

	agent := &ClaudeCodeAgent{}
	count, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallUserHooks() installed nothing on a file without Entire hooks")
	}

	raw := readRawSettings(t, userSettingsTestPath(t, home))
	if got := string(raw["permissions"]); got != `"deny-all"` {
		t.Errorf("permissions not preserved verbatim: got %s, want %q", got, `"deny-all"`)
	}
}

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
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || !ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after install, want (true, nil)", ok, err)
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

	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after uninstall, want (false, nil)", ok, err)
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

// TestUserAuthoredEntireCLIHookSurvivesInstallAndUninstall pins verb-scoped
// hook recognition: a USER-AUTHORED hook that merely invokes the entire
// binary (`entire status ...`) must never be claimed as ours — an earlier
// prefix (`entire `) matched it, so UninstallUserHooks deleted it.
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
	cmds := collectHookCommands(t, userSettingsTestPath(t, home))
	if got := countOf(cmds, userCmd); got != 2 {
		t.Fatalf("user-authored entire-CLI hook count after install = %d, want 2 (commands: %v)", got, cmds)
	}

	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}
	cmds = collectHookCommands(t, userSettingsTestPath(t, home))
	if got := countOf(cmds, userCmd); got != 2 {
		t.Errorf("user-authored entire-CLI hook count after uninstall = %d, want 2 (commands: %v)", got, cmds)
	}
	for _, c := range cmds {
		if isEntireHook(c) {
			t.Errorf("Entire hook left behind after uninstall: %s", c)
		}
	}
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

// TestInstallUserHooks_RepairsAlternateFormEntireHook pins the user-level
// self-heal: a pre-existing Entire hook in a different command form (bare
// `entire hooks claude-code stop`) must be replaced, not joined — the
// exact-string dedup check otherwise appended a second wrapped entry and both
// fired on every Stop machine-wide.
func TestInstallUserHooks_RepairsAlternateFormEntireHook(t *testing.T) {
	home := setUserHome(t)
	writeUserClaudeSettings(t, home, `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]}]
  }
}`)

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}

	raw := readRawSettings(t, userSettingsTestPath(t, home))
	var hooks map[string][]ClaudeHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	for hookType, matchers := range hooks {
		entireEntries := 0
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if isEntireHook(h.Command) {
					entireEntries++
				}
			}
		}
		want := 1
		if hookType == "PostToolUse" {
			want = 2 // post-task (Agent) + post-todo (TaskCreate|TaskUpdate)
		}
		if entireEntries != want {
			t.Errorf("hook type %s has %d Entire entries after install, want exactly %d", hookType, entireEntries, want)
		}
	}
	if got := countOf(collectHookCommands(t, userSettingsTestPath(t, home)), "entire hooks claude-code stop"); got != 0 {
		t.Errorf("bare-form Entire hook still present after repair (count %d)", got)
	}
}

// TestInstallUserHooks_UnparseableHookSectionErrorsCleanly pins item "never
// clobber unparseable hook sections": a managed hook section in an unexpected
// shape must abort install AND uninstall with an error naming the section,
// leaving the file byte-identical — the old parse path silently treated it as
// empty, clobbering it on install and deleting it on disable --global.
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

// TestInstallUserHooks_WritesThroughSymlink pins the dotfile-manager case: a
// symlinked ~/.claude/settings.json must be rewritten through to its target —
// a naive temp+rename write replaces the link with a regular file, silently
// detaching the managed target.
func TestInstallUserHooks_WritesThroughSymlink(t *testing.T) {
	home := setUserHome(t)
	target := filepath.Join(home, "dotfiles", "claude-settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := userSettingsTestPath(t, home)
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("install replaced the settings.json symlink with a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "entire hooks claude-code stop") || !strings.Contains(string(data), `"opus"`) {
		t.Errorf("symlink target not rewritten in place: %s", data)
	}
}

// TestAreUserHooksInstalled_PartialInstallReadsAsNotInstalled pins the
// alignment of the user-level predicate with the repo-level completeness spec
// (CheckHookConfig): a Stop-only file must read as not-installed so doctor
// flags it and the idempotent installer repairs it, instead of "installed"
// masking hooks that never fire.
func TestAreUserHooksInstalled_PartialInstallReadsAsNotInstalled(t *testing.T) {
	home := setUserHome(t)
	agent := &ClaudeCodeAgent{}
	stopOnly := `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": ` + mustJSON(t, agentWrappedStopCmd()) + `}]}]
  }
}`
	writeUserClaudeSettings(t, home, stopOnly)

	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || ok {
		t.Fatalf("Stop-only file: AreUserHooksInstalled() = (%v, %v), want (false, nil)", ok, err)
	}
	count, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallUserHooks() must repair a partial install, reported nothing to do")
	}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || !ok {
		t.Fatalf("after repair: AreUserHooksInstalled() = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestAreUserHooksInstalled_ReadErrorSurfaces pins honest error
// classification: an unreadable config is "cannot tell", not "not installed"
// — and the installer must refuse rather than replace the file (verified
// failure mode: EACCES read → config silently replaced, doctor reported OK).
func TestAreUserHooksInstalled_ReadErrorSurfaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadability is not enforceable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	home := setUserHome(t)
	writeUserClaudeSettings(t, home, `{"model":"opus"}`)
	path := userSettingsTestPath(t, home)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Logf("restore permissions: %v", err)
		}
	})

	agent := &ClaudeCodeAgent{}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err == nil || ok {
		t.Fatalf("unreadable file: AreUserHooksInstalled() = (%v, %v), want (false, error)", ok, err)
	}
	if _, err := agent.InstallUserHooks(context.Background()); err == nil {
		t.Fatal("InstallUserHooks() must refuse to replace an unreadable settings file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"model":"opus"}` {
		t.Fatalf("unreadable file must be left untouched (data=%q err=%v)", data, err)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
