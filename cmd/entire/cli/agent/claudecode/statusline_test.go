package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// prodStatuslineCommand is the production statusLine command Entire installs.
const prodStatuslineCommand = "entire hooks claude-code statusline"

// readStatusLine reads the statusLine block from <dir>/.claude/settings.json.
// Returns ok=false when the key is absent.
func readStatusLine(t *testing.T, dir string) (claudeStatusLine, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".claude", ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	slRaw, ok := raw["statusLine"]
	if !ok {
		return claudeStatusLine{}, false
	}
	var sl claudeStatusLine
	if err := json.Unmarshal(slRaw, &sl); err != nil {
		t.Fatalf("parse statusLine: %v", err)
	}
	return sl, true
}

func TestInstallStatusline_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	ag := &ClaudeCodeAgent{}
	changed, err := ag.InstallStatusline(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallStatusline() error = %v", err)
	}
	if !changed {
		t.Fatal("expected changed = true on fresh install")
	}

	sl, ok := readStatusLine(t, dir)
	if !ok {
		t.Fatal("statusLine not written")
	}
	if sl.Type != "command" {
		t.Errorf("statusLine.type = %q, want command", sl.Type)
	}
	if sl.Command != prodStatuslineCommand {
		t.Errorf("statusLine.command = %q, want production command", sl.Command)
	}
	if !ag.IsStatuslineInstalled(context.Background()) {
		t.Error("IsStatuslineInstalled() = false after install")
	}
}

func TestInstallStatusline_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallStatusline(context.Background(), false, false); err != nil {
		t.Fatalf("first InstallStatusline() error = %v", err)
	}
	changed, err := ag.InstallStatusline(context.Background(), false, false)
	if err != nil {
		t.Fatalf("second InstallStatusline() error = %v", err)
	}
	if changed {
		t.Error("expected changed = false on idempotent re-install")
	}
}

func TestInstallStatusline_LocalDevRewrite(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallStatusline(context.Background(), false, false); err != nil {
		t.Fatalf("prod InstallStatusline() error = %v", err)
	}
	changed, err := ag.InstallStatusline(context.Background(), true, false)
	if err != nil {
		t.Fatalf("local-dev InstallStatusline() error = %v", err)
	}
	if !changed {
		t.Error("expected changed = true when switching prod -> local-dev")
	}
	sl, _ := readStatusLine(t, dir)
	if sl.Command != localDevHookCmdPrefix+"hooks claude-code statusline" {
		t.Errorf("statusLine.command = %q, want local-dev command", sl.Command)
	}
}

func TestInstallStatusline_ForeignNotClobbered(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeForeignSettings(t, dir, `{"statusLine":{"type":"command","command":"my-custom.sh"}}`)

	ag := &ClaudeCodeAgent{}
	changed, err := ag.InstallStatusline(context.Background(), false, false)
	if !errors.Is(err, ErrStatuslineConflict) {
		t.Fatalf("InstallStatusline() error = %v, want ErrStatuslineConflict", err)
	}
	if changed {
		t.Error("expected changed = false when refusing to clobber")
	}
	sl, _ := readStatusLine(t, dir)
	if sl.Command != "my-custom.sh" {
		t.Errorf("foreign statusLine was modified: command = %q", sl.Command)
	}
}

func TestInstallStatusline_ForceReplacesForeign(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeForeignSettings(t, dir, `{"statusLine":{"type":"command","command":"my-custom.sh"}}`)

	ag := &ClaudeCodeAgent{}
	changed, err := ag.InstallStatusline(context.Background(), false, true)
	if err != nil {
		t.Fatalf("InstallStatusline(force) error = %v", err)
	}
	if !changed {
		t.Error("expected changed = true when forcing replacement")
	}
	sl, _ := readStatusLine(t, dir)
	if sl.Command != prodStatuslineCommand {
		t.Errorf("statusLine.command = %q, want Entire command after force", sl.Command)
	}
}

func TestInstallStatusline_PreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeForeignSettings(t, dir, `{"model":"opus","permissions":{"deny":["Read(./secret)"]}}`)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallStatusline(context.Background(), false, false); err != nil {
		t.Fatalf("InstallStatusline() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	if _, ok := raw["model"]; !ok {
		t.Error("model key was dropped")
	}
	if _, ok := raw["permissions"]; !ok {
		t.Error("permissions key was dropped")
	}
	if _, ok := raw["statusLine"]; !ok {
		t.Error("statusLine key was not added")
	}
}

func TestUninstallStatusline_RemovesEntireOwned(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallStatusline(context.Background(), false, false); err != nil {
		t.Fatalf("InstallStatusline() error = %v", err)
	}
	if err := ag.UninstallStatusline(context.Background()); err != nil {
		t.Fatalf("UninstallStatusline() error = %v", err)
	}
	if _, ok := readStatusLine(t, dir); ok {
		t.Error("statusLine still present after uninstall")
	}
}

func TestUninstallStatusline_LeavesForeign(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeForeignSettings(t, dir, `{"statusLine":{"type":"command","command":"my-custom.sh"}}`)

	ag := &ClaudeCodeAgent{}
	if err := ag.UninstallStatusline(context.Background()); err != nil {
		t.Fatalf("UninstallStatusline() error = %v", err)
	}
	sl, ok := readStatusLine(t, dir)
	if !ok || sl.Command != "my-custom.sh" {
		t.Errorf("foreign statusLine was removed: ok=%v command=%q", ok, sl.Command)
	}
}

func TestUninstallHooks_RemovesStatusline(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	ag := &ClaudeCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if _, err := ag.InstallStatusline(context.Background(), false, false); err != nil {
		t.Fatalf("InstallStatusline() error = %v", err)
	}
	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}
	if _, ok := readStatusLine(t, dir); ok {
		t.Error("statusLine should be removed by UninstallHooks")
	}
}

// writeForeignSettings writes raw JSON to <dir>/.claude/settings.json.
func writeForeignSettings(t *testing.T, dir, content string) {
	t.Helper()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o750); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, ClaudeSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}
