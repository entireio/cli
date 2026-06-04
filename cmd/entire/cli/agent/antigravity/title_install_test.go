package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgySettingsFile writes content to <dir>/settings.json.
func writeAgySettingsFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("writeAgySettingsFile: mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("writeAgySettingsFile: write: %v", err)
	}
}

// readTitleCommand parses <dir>/settings.json and returns the title.command value,
// or "" if the file is absent or the key is not present.
func readTitleCommand(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		Title *struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"title"`
		Theme string `json:"theme"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("readTitleCommand: unmarshal: %v", err)
	}
	if s.Title == nil {
		return ""
	}
	return s.Title.Command
}

// mustJSON marshals s to a JSON string value (quoted).
func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return b
}

func TestInstallTitle_FreshConfig(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	if err := InstallTitleTee(false); err != nil {
		t.Fatalf("InstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	want := "entire hooks antigravity title-tee"
	if got != want {
		t.Errorf("title.command = %q, want %q", got, want)
	}
}

func TestInstallTitle_WrapsExistingCommand(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	writeAgySettingsFile(t, cfgDir, `{"theme":"dark","title":{"type":"command","command":"~/bin/my-status.sh"}}`)

	if err := InstallTitleTee(false); err != nil {
		t.Fatalf("InstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	want := "entire hooks antigravity title-tee --wrap '~/bin/my-status.sh'"
	if got != want {
		t.Errorf("title.command = %q, want %q", got, want)
	}

	// Unknown-key preservation: "theme" must still be present in raw file.
	raw, err := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(raw), `"theme"`) {
		t.Error(`settings.json lost "theme" key after install`)
	}
}

func TestInstallTitle_Idempotent(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	if err := InstallTitleTee(false); err != nil {
		t.Fatalf("first InstallTitleTee: %v", err)
	}
	first := readTitleCommand(t, cfgDir)

	if err := InstallTitleTee(false); err != nil {
		t.Fatalf("second InstallTitleTee: %v", err)
	}
	second := readTitleCommand(t, cfgDir)

	if first != second {
		t.Errorf("idempotency: first=%q second=%q", first, second)
	}
}

func TestUninstallTitle_RestoresOriginal(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	writeAgySettingsFile(t, cfgDir, `{"title":{"type":"command","command":"entire hooks antigravity title-tee --wrap '~/bin/my-status.sh'"}}`)

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	want := "~/bin/my-status.sh"
	if got != want {
		t.Errorf("title.command after uninstall = %q, want %q", got, want)
	}
}

func TestUninstallTitle_RemovesBareTee(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	writeAgySettingsFile(t, cfgDir, `{"title":{"type":"command","command":"entire hooks antigravity title-tee"}}`)

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	if got != "" {
		t.Errorf("title.command after bare-tee uninstall = %q, want empty", got)
	}
}

func TestUninstallTitle_LeavesForeignCommandAlone(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	writeAgySettingsFile(t, cfgDir, `{"title":{"type":"command","command":"~/bin/my-status.sh"}}`)

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	want := "~/bin/my-status.sh"
	if got != want {
		t.Errorf("title.command after foreign-command uninstall = %q, want %q", got, want)
	}
}

func TestUninstallTitle_LeavesUserWrappedTeeAlone(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	// User-authored wrapper that happens to contain the marker string.
	const cmd = "my-wrapper.sh 'entire hooks antigravity title-tee'"
	writeAgySettingsFile(t, cfgDir, `{"title":{"type":"command","command":"`+cmd+`"}}`)

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	if got != cmd {
		t.Errorf("title.command = %q, want %q (user wrapper should be left alone)", got, cmd)
	}
}

func TestUninstallTitle_LeavesMalformedWrapAlone(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	// Contains the marker + a --wrap flag but unquoted (malformed) — safer to leave than delete.
	const cmd = "entire hooks antigravity title-tee --wrap unquoted"
	writeAgySettingsFile(t, cfgDir, `{"title":{"type":"command","command":"`+cmd+`"}}`)

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	if got != cmd {
		t.Errorf("title.command = %q, want %q (malformed wrap should be left alone)", got, cmd)
	}
}

func TestInstallTitle_WrapsCommandContainingWrapSubstring(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	// Original command itself contains "--wrap fancy" — must survive round-trip.
	const original = "~/bin/title.sh --wrap fancy"
	origJSON := mustJSON(t, original)
	content := `{"title":{"type":"command","command":` + string(origJSON) + `}}`
	writeAgySettingsFile(t, cfgDir, content)

	if err := InstallTitleTee(false); err != nil {
		t.Fatalf("InstallTitleTee: %v", err)
	}

	// Installed command should wrap the original.
	installed := readTitleCommand(t, cfgDir)
	want := "entire hooks antigravity title-tee --wrap '~/bin/title.sh --wrap fancy'"
	if installed != want {
		t.Errorf("installed title.command = %q, want %q", installed, want)
	}

	// Uninstall should restore the original exactly.
	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}
	got := readTitleCommand(t, cfgDir)
	if got != original {
		t.Errorf("round-trip: got %q, want %q", got, original)
	}
}

func TestUninstallTitle_RemovesBareLocalDevTee(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	// Exactly the localDev tee command — must be removed.
	localDevCmd := titleTeeCommand(true, "")
	cmdJSON := mustJSON(t, localDevCmd)
	content := `{"title":{"type":"command","command":` + string(cmdJSON) + `}}`
	writeAgySettingsFile(t, cfgDir, content)

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	if got != "" {
		t.Errorf("title.command after localDev bare-tee uninstall = %q, want empty", got)
	}
}

func TestInstallUninstall_RoundTripsEmbeddedSingleQuotes(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	original := `echo 'hi there' | awk '{print $1}'`
	origJSON := mustJSON(t, original)
	content := `{"title":{"type":"command","command":` + string(origJSON) + `}}`
	writeAgySettingsFile(t, cfgDir, content)

	if err := InstallTitleTee(false); err != nil {
		t.Fatalf("InstallTitleTee: %v", err)
	}
	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	got := readTitleCommand(t, cfgDir)
	if got != original {
		t.Errorf("round-trip: got %q, want %q", got, original)
	}
}
