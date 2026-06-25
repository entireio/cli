package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readStatusLineForTest reads the statusLine entry from a temp dir's
// .claude/settings.json. ok is false when no statusLine key is present.
func readStatusLineForTest(t *testing.T, dir string) (sl claudeStatusLine, present bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".claude", ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	entry, ok := raw["statusLine"]
	if !ok {
		return claudeStatusLine{}, false
	}
	if err := json.Unmarshal(entry, &sl); err != nil {
		t.Fatalf("parse statusLine: %v", err)
	}
	return sl, true
}

func TestStatusLineCommand(t *testing.T) {
	t.Parallel()
	if got := statusLineCommand(false); got != statusLineProductionCommand {
		t.Errorf("statusLineCommand(false) = %q, want %q", got, statusLineProductionCommand)
	}
	got := statusLineCommand(true)
	if got == statusLineProductionCommand {
		t.Errorf("local-dev command should differ from production, got %q", got)
	}
	if !isEntireStatusLineCommand(got) {
		t.Errorf("local-dev command %q must be recognized as Entire's", got)
	}
}

func TestIsEntireStatusLineCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command string
		want    bool
	}{
		{"entire trail status", true},
		{"entire trail status --format statusline", true},
		{"${CLAUDE_PROJECT_DIR}/scripts/entire-dev trail status", true},
		{"my-custom-statusline.sh", false},
		{"entire status", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isEntireStatusLineCommand(tc.command); got != tc.want {
			t.Errorf("isEntireStatusLineCommand(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestInstallStatusLine_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	installed, err := (&ClaudeCodeAgent{}).InstallStatusLine(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallStatusLine() error = %v", err)
	}
	if !installed {
		t.Fatal("InstallStatusLine() = false, want true on fresh install")
	}
	sl, ok := readStatusLineForTest(t, tempDir)
	if !ok {
		t.Fatal("statusLine key not written")
	}
	if sl.Type != "command" || sl.Command != statusLineProductionCommand {
		t.Errorf("statusLine = %+v, want type=command command=%q", sl, statusLineProductionCommand)
	}
}

func TestInstallStatusLine_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agent := &ClaudeCodeAgent{}

	if _, err := agent.InstallStatusLine(context.Background(), false); err != nil {
		t.Fatalf("first InstallStatusLine() error = %v", err)
	}
	installed, err := agent.InstallStatusLine(context.Background(), false)
	if err != nil {
		t.Fatalf("second InstallStatusLine() error = %v", err)
	}
	if installed {
		t.Error("second InstallStatusLine() = true, want false (already installed)")
	}
}

func TestInstallStatusLine_PreservesUserStatusLine(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeSettings(t, tempDir, `{"statusLine":{"type":"command","command":"my-custom.sh"}}`)

	installed, err := (&ClaudeCodeAgent{}).InstallStatusLine(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallStatusLine() error = %v", err)
	}
	if installed {
		t.Error("InstallStatusLine() = true, want false (user status line present)")
	}
	sl, _ := readStatusLineForTest(t, tempDir)
	if sl.Command != "my-custom.sh" {
		t.Errorf("user status line was overwritten: %+v", sl)
	}
}

func TestInstallStatusLine_PreservesOtherKeys(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeSettings(t, tempDir, `{"model":"opus","permissions":{"deny":["Read(./secret)"]}}`)

	if _, err := (&ClaudeCodeAgent{}).InstallStatusLine(context.Background(), false); err != nil {
		t.Fatalf("InstallStatusLine() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tempDir, ".claude", ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	for _, key := range []string{"model", "permissions", "statusLine"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("settings.json missing key %q after install", key)
		}
	}
}

func TestInstallStatusLine_UpgradesOwnCommand(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	// A stale Entire command (local-dev form) should be upgraded to production.
	writeSettings(t, tempDir, `{"statusLine":{"type":"command","command":"`+statusLineCommand(true)+`"}}`)

	installed, err := (&ClaudeCodeAgent{}).InstallStatusLine(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallStatusLine() error = %v", err)
	}
	if !installed {
		t.Error("InstallStatusLine() = false, want true when upgrading Entire's own command")
	}
	sl, _ := readStatusLineForTest(t, tempDir)
	if sl.Command != statusLineProductionCommand {
		t.Errorf("command not upgraded: got %q", sl.Command)
	}
}

func TestUninstallStatusLine_RemovesOurs(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agent := &ClaudeCodeAgent{}
	if _, err := agent.InstallStatusLine(context.Background(), false); err != nil {
		t.Fatalf("InstallStatusLine() error = %v", err)
	}
	if !agent.IsStatusLineInstalled(context.Background()) {
		t.Fatal("expected status line installed")
	}
	if err := agent.UninstallStatusLine(context.Background()); err != nil {
		t.Fatalf("UninstallStatusLine() error = %v", err)
	}
	if _, ok := readStatusLineForTest(t, tempDir); ok {
		t.Error("statusLine key still present after uninstall")
	}
	if agent.IsStatusLineInstalled(context.Background()) {
		t.Error("IsStatusLineInstalled() = true after uninstall")
	}
}

func TestUninstallStatusLine_PreservesUserStatusLine(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	writeSettings(t, tempDir, `{"statusLine":{"type":"command","command":"my-custom.sh"}}`)

	if err := (&ClaudeCodeAgent{}).UninstallStatusLine(context.Background()); err != nil {
		t.Fatalf("UninstallStatusLine() error = %v", err)
	}
	sl, ok := readStatusLineForTest(t, tempDir)
	if !ok || sl.Command != "my-custom.sh" {
		t.Errorf("user status line was removed: present=%v sl=%+v", ok, sl)
	}
}

func TestIsStatusLineInstalled_NoSettings(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	if (&ClaudeCodeAgent{}).IsStatusLineInstalled(context.Background()) {
		t.Error("IsStatusLineInstalled() = true with no settings file")
	}
}

// writeSettings writes a .claude/settings.json with the given JSON body.
func writeSettings(t *testing.T, dir, body string) {
	t.Helper()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o750); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, ClaudeSettingsFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}
