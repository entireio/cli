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
	res, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if res.Installed == 0 {
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
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || !ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after install, want (true, nil)", ok, err)
	}
}

func TestInstallUserHooks_Gemini_Idempotent(t *testing.T) {
	home := setUserHome(t)

	agent := &GeminiCLIAgent{}
	first, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("first InstallUserHooks() error = %v", err)
	}
	if first.Installed == 0 {
		t.Fatal("first install must report installed hooks")
	}
	if first.Repaired {
		t.Error("a fresh install must not report a repair")
	}
	before, err := os.ReadFile(userGeminiSettingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("second InstallUserHooks() error = %v", err)
	}
	if second.Installed != 0 || second.Repaired {
		t.Errorf("second install must be a no-op, got %+v", second)
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

	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after uninstall, want (false, nil)", ok, err)
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

// TestUserAuthoredEntireCLIHookSurvives_Gemini pins verb-scoped hook
// recognition: a USER-AUTHORED hook whose command merely invokes the entire
// binary must survive both install (whose remove-then-add cycle deleted it
// under the old `entire ` prefix on a plain enable) and uninstall.
func TestUserAuthoredEntireCLIHookSurvives_Gemini(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	const userCmd = "entire status --json > /tmp/entire-status.json"
	existing := `{
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "my-status", "type": "command", "command": "` + userCmd + `"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	raw := readRawGeminiSettings(t, path)
	if !strings.Contains(string(raw["hooks"]), userCmd) {
		t.Fatalf("install removed the user-authored entire-CLI hook: %s", raw["hooks"])
	}

	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}
	raw = readRawGeminiSettings(t, path)
	if !strings.Contains(string(raw["hooks"]), userCmd) {
		t.Errorf("uninstall removed the user-authored entire-CLI hook: %s", raw["hooks"])
	}
	if strings.Contains(string(raw["hooks"]), "entire hooks gemini") {
		t.Errorf("Entire hooks left behind: %s", raw["hooks"])
	}
}

// TestInstallUserHooks_Gemini_PreservesUnknownKeysInHooksConfigAndHooks pins
// the "preserves every unrelated key" contract: unknown members of
// hooksConfig (formerly decoded into a struct holding only `enabled`) and
// non-array members of hooks (formerly stripped wholesale) must round-trip.
func TestInstallUserHooks_Gemini_PreservesUnknownKeysInHooksConfigAndHooks(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "hooksConfig": {"enabled": true, "timeout": 30, "customField": "x"},
  "hooks": {
    "myOwnMap": {"a": 1}
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}

	raw := readRawGeminiSettings(t, path)
	var hc map[string]json.RawMessage
	if err := json.Unmarshal(raw["hooksConfig"], &hc); err != nil {
		t.Fatalf("parse hooksConfig: %v", err)
	}
	if string(hc["enabled"]) != "true" {
		t.Errorf("hooksConfig.enabled = %s, want true", hc["enabled"])
	}
	if string(hc["timeout"]) != "30" || string(hc["customField"]) != `"x"` {
		t.Errorf("hooksConfig unknown keys dropped: %s", raw["hooksConfig"])
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	var myOwnMap map[string]int
	if err := json.Unmarshal(hooks["myOwnMap"], &myOwnMap); err != nil || myOwnMap["a"] != 1 {
		t.Errorf("non-array hooks member dropped or rewritten (err=%v): %s", err, hooks["myOwnMap"])
	}
}

// TestInstallUserHooks_Gemini_UnparseableHookSectionErrorsCleanly: a managed
// hook section in an unexpected shape must abort install and uninstall with
// an error naming the section, leaving the file byte-identical.
func TestInstallUserHooks_Gemini_UnparseableHookSectionErrorsCleanly(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	original := `{
  "hooks": {
    "SessionStart": {"unexpected": "shape"}
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	_, err := agent.InstallUserHooks(context.Background())
	if err == nil {
		t.Fatal("InstallUserHooks() must error on an unparseable hook section")
	}
	if !strings.Contains(err.Error(), "SessionStart") || !strings.Contains(err.Error(), path) {
		t.Errorf("install error must name the section and file, got: %v", err)
	}
	if err := agent.UninstallUserHooks(context.Background()); err == nil {
		t.Fatal("UninstallUserHooks() must error on an unparseable hook section")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != original {
		t.Errorf("refused write must leave the file byte-identical (err=%v):\n%s", readErr, data)
	}
}

// collectGeminiHookEntries returns every hook entry in the managed array
// sections of the settings file at path.
func collectGeminiHookEntries(t *testing.T, path string) []GeminiHookEntry {
	t.Helper()
	raw := readRawGeminiSettings(t, path)
	hooksRaw, ok := raw["hooks"]
	if !ok {
		return nil
	}
	var hooks map[string][]GeminiHookMatcher
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	var entries []GeminiHookEntry
	for _, matchers := range hooks {
		for _, m := range matchers {
			entries = append(entries, m.Hooks...)
		}
	}
	return entries
}

func productionCommandFor(t *testing.T, name string) string {
	t.Helper()
	for _, spec := range geminiHookSpecs {
		if spec.name == name {
			return spec.productionCommand()
		}
	}
	t.Fatalf("no gemini hook spec named %s", name)
	return ""
}

// TestUserAuthoredEntryWithEntireNameSurvives_Gemini pins narrowed name-based
// claiming: a user-authored hook that reused an Entire entry name as a
// template but points at their own command must never be claimed as ours —
// pure name matching silently deleted it on install (remove-then-add) and on
// uninstall.
func TestUserAuthoredEntryWithEntireNameSurvives_Gemini(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	const userCmd = "/home/me/bin/my-guard.sh"
	existing := `{
  "hooks": {
    "BeforeTool": [{"matcher": "*", "hooks": [{"name": "entire-before-tool", "type": "command", "command": "` + userCmd + `"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	survived := false
	for _, e := range collectGeminiHookEntries(t, path) {
		if e.Command == userCmd {
			survived = true
		}
	}
	if !survived {
		t.Fatal("install deleted the user-authored entry that reused an Entire hook name")
	}

	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}
	survived = false
	for _, e := range collectGeminiHookEntries(t, path) {
		if e.Command == userCmd {
			survived = true
		}
		if isEntireHookEntry(e) {
			t.Errorf("Entire hook left behind after uninstall: %+v", e)
		}
	}
	if !survived {
		t.Error("uninstall deleted the user-authored entry that reused an Entire hook name")
	}
}

// TestInstallUserHooks_Gemini_RepairsLegacyGoRunEntry pins the legacy rescue:
// an entry written by an old local-dev install (the `go run
// ${GEMINI_PROJECT_DIR}` form) must still be recognized as Entire's — now via
// the command prefix list rather than name-based claiming — and repaired to
// exactly one current-form entry.
func TestInstallUserHooks_Gemini_RepairsLegacyGoRunEntry(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	const legacyCmd = "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-start"
	existing := `{
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "entire-session-start", "type": "command", "command": "` + legacyCmd + `"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	res, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if !res.Repaired {
		t.Error("replacing a legacy go-run entry must be reported as a repair")
	}

	current := productionCommandFor(t, "entire-session-start")
	var sessionStartCount int
	for _, e := range collectGeminiHookEntries(t, path) {
		if e.Command == legacyCmd {
			t.Errorf("legacy go-run entry still present after repair: %+v", e)
		}
		if e.Name == "entire-session-start" {
			sessionStartCount++
			if e.Command != current {
				t.Errorf("session-start command after repair = %q, want current form %q", e.Command, current)
			}
		}
	}
	if sessionStartCount != 1 {
		t.Errorf("entire-session-start entry count after repair = %d, want exactly 1", sessionStartCount)
	}
}

// TestAreUserHooksInstalled_Gemini_PartialInstallReadsAsNotInstalled mirrors
// the Claude Code completeness predicate: a user file with the SessionStart
// entry intact but every other Entire entry deleted must read as
// not-installed (status/doctor honesty) and the install must repair it
// instead of short-circuiting on the surviving SessionStart entry.
func TestAreUserHooksInstalled_Gemini_PartialInstallReadsAsNotInstalled(t *testing.T) {
	home := setUserHome(t)
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	cmdJSON, err := json.Marshal(productionCommandFor(t, "entire-session-start"))
	if err != nil {
		t.Fatal(err)
	}
	existing := `{
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "entire-session-start", "type": "command", "command": ` + string(cmdJSON) + `}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &GeminiCLIAgent{}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || ok {
		t.Fatalf("partial file: AreUserHooksInstalled() = (%v, %v), want (false, nil)", ok, err)
	}
	res, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if !res.Repaired {
		t.Error("repairing a partial install must be reported as a repair")
	}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || !ok {
		t.Fatalf("after repair: AreUserHooksInstalled() = (%v, %v), want (true, nil)", ok, err)
	}
	counts := make(map[string]int)
	for _, e := range collectGeminiHookEntries(t, path) {
		if isEntireHookEntry(e) {
			counts[e.Name]++
		}
	}
	for name := range entireGeminiHookNames {
		if counts[name] != 1 {
			t.Errorf("entry %s count after repair = %d, want exactly 1", name, counts[name])
		}
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
