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

func writeUserGeminiSettings(t *testing.T, home, content string) string {
	t.Helper()
	path := userGeminiSettingsPath(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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

// TestUserHooks_Gemini_Lifecycle is the anchor walk: install into a populated
// user file (merge semantics, production forms), re-install (byte-identical
// no-op), then uninstall (only Entire's entries removed).
func TestUserHooks_Gemini_Lifecycle(t *testing.T) {
	home := setUserHome(t)
	agent := &GeminiCLIAgent{}
	// A missing file is not an uninstall error.
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() on missing file = %v", err)
	}
	path := writeUserGeminiSettings(t, home, `{
  "theme": "GitHub",
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "my-hook", "type": "command", "command": "my-own-session-start"}]}]
  }
}`)

	// Install: merge, never clobber, production command forms only.
	res, err := agent.InstallUserHooks(context.Background())
	if err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}
	if res.Installed == 0 || res.Repaired {
		t.Fatalf("fresh install must report installs and no repair, got %+v", res)
	}
	raw := readRawGeminiSettings(t, path)
	if got := string(raw["theme"]); got != `"GitHub"` {
		t.Errorf("theme clobbered: %s", got)
	}
	if !strings.Contains(string(raw["hooksConfig"]), "true") {
		t.Errorf("hooksConfig.enabled not set: %s", raw["hooksConfig"])
	}
	content := string(raw["hooks"])
	if !strings.Contains(content, "my-own-session-start") || !strings.Contains(content, "entire hooks gemini session-start") {
		t.Errorf("merge lost a hook: %s", content)
	}
	if strings.Contains(content, "entire-dev") || strings.Contains(content, "git rev-parse") {
		t.Errorf("user-level hooks use a repo-local command form: %s", content)
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

	// Uninstall: only Entire's entries go.
	if err := agent.UninstallUserHooks(context.Background()); err != nil {
		t.Fatalf("UninstallUserHooks() error = %v", err)
	}
	if ok, err := agent.AreUserHooksInstalled(context.Background()); err != nil || ok {
		t.Errorf("AreUserHooksInstalled() = (%v, %v) after uninstall, want (false, nil)", ok, err)
	}
	raw = readRawGeminiSettings(t, path)
	if got := string(raw["theme"]); got != `"GitHub"` {
		t.Errorf("theme clobbered by uninstall: %s", got)
	}
	content = string(raw["hooks"])
	if !strings.Contains(content, "my-own-session-start") {
		t.Error("uninstall removed the user's own hook")
	}
	if strings.Contains(content, "entire hooks gemini") {
		t.Errorf("Entire hooks left behind: %s", content)
	}
}

// User-authored entries are never claimed as ours — neither by command (a hook
// that merely invokes the entire binary) nor by a reused Entire entry NAME
// pointing at the user's own command; both were once deleted by the
// remove-then-add cycle.
func TestUserAuthoredEntriesSurvive_Gemini(t *testing.T) {
	cases := []struct {
		name, existing, userCmd string
	}{
		{
			"entire-CLI command under a user name",
			`{"hooks":{"SessionStart":[{"hooks":[{"name":"my-status","type":"command","command":"entire status --json > /tmp/entire-status.json"}]}]}}`,
			"entire status --json > /tmp/entire-status.json",
		},
		{
			"Entire entry name over a user command",
			`{"hooks":{"BeforeTool":[{"matcher":"*","hooks":[{"name":"entire-before-tool","type":"command","command":"/home/me/bin/my-guard.sh"}]}]}}`,
			"/home/me/bin/my-guard.sh",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := setUserHome(t)
			path := writeUserGeminiSettings(t, home, c.existing)

			agent := &GeminiCLIAgent{}
			if _, err := agent.InstallUserHooks(context.Background()); err != nil {
				t.Fatalf("InstallUserHooks() error = %v", err)
			}
			if !strings.Contains(string(readRawGeminiSettings(t, path)["hooks"]), c.userCmd) {
				t.Fatal("install deleted the user-authored entry")
			}
			if err := agent.UninstallUserHooks(context.Background()); err != nil {
				t.Fatalf("UninstallUserHooks() error = %v", err)
			}
			survived := false
			for _, e := range collectGeminiHookEntries(t, path) {
				if e.Command == c.userCmd {
					survived = true
				}
				if isEntireHookEntry(e) {
					t.Errorf("Entire hook left behind after uninstall: %+v", e)
				}
			}
			if !survived {
				t.Error("uninstall deleted the user-authored entry")
			}
		})
	}
}

// Unknown members of hooksConfig and non-array members of hooks must
// round-trip (both were once stripped by struct-shaped decoding).
func TestInstallUserHooks_Gemini_PreservesUnknownKeysInHooksConfigAndHooks(t *testing.T) {
	home := setUserHome(t)
	path := writeUserGeminiSettings(t, home, `{
  "hooksConfig": {"enabled": true, "timeout": 30, "customField": "x"},
  "hooks": {
    "myOwnMap": {"a": 1}
  }
}`)

	agent := &GeminiCLIAgent{}
	if _, err := agent.InstallUserHooks(context.Background()); err != nil {
		t.Fatalf("InstallUserHooks() error = %v", err)
	}

	raw := readRawGeminiSettings(t, path)
	var hc map[string]json.RawMessage
	if err := json.Unmarshal(raw["hooksConfig"], &hc); err != nil {
		t.Fatalf("parse hooksConfig: %v", err)
	}
	if string(hc["enabled"]) != "true" || string(hc["timeout"]) != "30" || string(hc["customField"]) != `"x"` {
		t.Errorf("hooksConfig keys dropped: %s", raw["hooksConfig"])
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

// A managed hook section in an unexpected shape aborts install and uninstall
// with an error naming the section, leaving the file byte-identical.
func TestInstallUserHooks_Gemini_UnparseableHookSectionErrorsCleanly(t *testing.T) {
	home := setUserHome(t)
	original := `{
  "hooks": {
    "SessionStart": {"unexpected": "shape"}
  }
}`
	path := writeUserGeminiSettings(t, home, original)

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

// Incomplete installs are recognized and repaired to exactly one current-form
// entry per hook: a legacy local-dev go-run entry (recognized via the command
// prefix list, not name claiming) and a partial file missing most entries.
func TestInstallUserHooks_Gemini_RepairsIncompleteInstalls(t *testing.T) {
	t.Run("legacy go-run entry is replaced", func(t *testing.T) {
		home := setUserHome(t)
		const legacyCmd = "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini session-start"
		path := writeUserGeminiSettings(t, home, `{
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "entire-session-start", "type": "command", "command": "`+legacyCmd+`"}]}]
  }
}`)
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
					t.Errorf("session-start command after repair = %q, want %q", e.Command, current)
				}
			}
		}
		if sessionStartCount != 1 {
			t.Errorf("entire-session-start entry count after repair = %d, want exactly 1", sessionStartCount)
		}
	})

	t.Run("partial install reads as not-installed and is completed", func(t *testing.T) {
		home := setUserHome(t)
		cmdJSON, err := json.Marshal(productionCommandFor(t, "entire-session-start"))
		if err != nil {
			t.Fatal(err)
		}
		path := writeUserGeminiSettings(t, home, `{
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "entire-session-start", "type": "command", "command": `+string(cmdJSON)+`}]}]
  }
}`)
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
	})
}
