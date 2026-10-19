package antigravity

import (
	"encoding/base64"
	"encoding/json"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	if err := InstallTitleTee(); err != nil {
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

	if err := InstallTitleTee(); err != nil {
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

// TestInstallTitle_WindowsHostWrapsExistingCommandBase64 pins the Windows
// form: agy runs the title slot through cmd.exe there, so the preserved
// original travels base64url-encoded rather than in POSIX single quotes, and
// uninstall restores it byte for byte. The name carries "Windows" so the
// windows-latest CI job selects it.
func TestInstallTitle_WindowsHostWrapsExistingCommandBase64(t *testing.T) {
	// No t.Parallel — uses t.Setenv and the process-global hook-host override.
	restore := agent.SetWindowsHookProbeForTesting("windows", nil)
	defer restore()
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	// Quotes, an ampersand and a percent: everything cmd.exe would tear apart.
	const original = `powershell -c "Write-Host 'x' & echo %CD%"`
	writeAgySettingsFile(t, cfgDir, `{"title":{"type":"command","command":`+string(mustJSON(t, original))+`}}`)

	if err := InstallTitleTee(); err != nil {
		t.Fatalf("InstallTitleTee: %v", err)
	}
	got := readTitleCommand(t, cfgDir)
	want := "entire hooks antigravity title-tee --wrap-b64 " + base64.RawURLEncoding.EncodeToString([]byte(original))
	if got != want {
		t.Errorf("title.command = %q, want %q", got, want)
	}
	if strings.ContainsAny(strings.TrimPrefix(got, "entire hooks antigravity title-tee --wrap-b64 "), `'"&%^|<>()=`) {
		t.Errorf("encoded form carries a cmd.exe metacharacter: %q", got)
	}

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}
	if got := readTitleCommand(t, cfgDir); got != original {
		t.Errorf("after uninstall title.command = %q, want the original %q", got, original)
	}
}

func TestExtractWrappedCommand_Forms(t *testing.T) {
	t.Parallel()
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		name    string
		command string
		want    string
		ok      bool
	}{
		{"quoted form", "entire hooks antigravity title-tee --wrap '~/bin/x.sh'", "~/bin/x.sh", true},
		{"base64 form", "entire hooks antigravity title-tee --wrap-b64 " + b64(`echo "a" & b`), `echo "a" & b`, true},
		// The quoted payload may itself mention the base64 flag; the quoted
		// form wins because it is parsed first.
		{"quoted payload naming the b64 flag", "entire hooks antigravity title-tee --wrap 'x --wrap-b64 abc'", "x --wrap-b64 abc", true},
		{"base64 token with trailing junk", "entire hooks antigravity title-tee --wrap-b64 " + b64("x") + " extra", "", false},
		{"base64 token not base64url", "entire hooks antigravity title-tee --wrap-b64 not*base64!", "", false},
		{"empty base64 token", "entire hooks antigravity title-tee --wrap-b64 ", "", false},
		{"bare tee", "entire hooks antigravity title-tee", "", false},
		{"unquoted legacy", "entire hooks antigravity title-tee --wrap unquoted", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := extractWrappedCommand(tc.command)
			if ok != tc.ok || got != tc.want {
				t.Errorf("extractWrappedCommand(%q) = (%q, %v), want (%q, %v)", tc.command, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestInstallTitle_Idempotent(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	if err := InstallTitleTee(); err != nil {
		t.Fatalf("first InstallTitleTee: %v", err)
	}
	first := readTitleCommand(t, cfgDir)

	if err := InstallTitleTee(); err != nil {
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

	if err := InstallTitleTee(); err != nil {
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

	// The bare local-dev tee command older versions wrote (local-dev mode was
	// removed; see isBareTitleTeeCommand) — must still be removed, from any path.
	localDevCmd := "go run '/some/other/worktree/cmd/entire/main.go' hooks antigravity title-tee"
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

	if err := InstallTitleTee(); err != nil {
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

// TestTitleTeeInstalled covers the three states the doctor check cares about:
// our marker present (true), no title key (false), and a foreign command (false).
func TestTitleTeeInstalled(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		cfgDir := t.TempDir()
		t.Setenv(configDirEnv, cfgDir)
		if err := InstallTitleTee(); err != nil {
			t.Fatalf("InstallTitleTee: %v", err)
		}
		if !TitleTeeInstalled() {
			t.Error("TitleTeeInstalled() = false, want true after InstallTitleTee")
		}
	})

	t.Run("absent", func(t *testing.T) {
		cfgDir := t.TempDir()
		t.Setenv(configDirEnv, cfgDir)
		// No settings.json at all.
		if TitleTeeInstalled() {
			t.Error("TitleTeeInstalled() = true, want false with no settings file")
		}
		// settings.json with no title key.
		writeAgySettingsFile(t, cfgDir, `{"theme":"dark"}`)
		if TitleTeeInstalled() {
			t.Error("TitleTeeInstalled() = true, want false with no title key")
		}
	})

	t.Run("foreign command", func(t *testing.T) {
		cfgDir := t.TempDir()
		t.Setenv(configDirEnv, cfgDir)
		writeAgySettingsFile(t, cfgDir,
			`{"title":{"type":"command","command":"my-own-title-script.sh"}}`)
		if TitleTeeInstalled() {
			t.Error("TitleTeeInstalled() = true, want false for a foreign command")
		}
	})
}

// TestUninstallTitleTee_LocalDevFromOtherWorktree pins the cross-worktree
// uninstall: a localDev bare tee installed from worktree A embeds A's absolute
// main.go path, and `entire agent remove antigravity` may run from worktree B
// or outside a repo. The bare-command check must match by SHAPE (go run …
// hooks antigravity title-tee), not by re-resolving the local path at
// uninstall time — otherwise the global title entry is silently orphaned and
// agy spawns a failing `go run` on every state change after A is deleted.
func TestUninstallTitleTee_LocalDevFromOtherWorktree(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)
	// Run from a non-repo CWD so localDevMainPath() cannot resolve the
	// original worktree's path.
	t.Chdir(t.TempDir())

	settings := `{"title":{"type":"command","command":"go run '/deleted/worktree-a/cmd/entire/main.go' hooks antigravity title-tee"}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["title"]; ok {
		t.Errorf("localDev bare tee from another worktree must be removed on uninstall, got %s", data)
	}
}

// TestUninstallTitleTee_UserWrapperLeftAlone pins the safety property the
// shape check must preserve: a user-authored wrapper that merely CONTAINS the
// tee command is not ours and must not be deleted.
func TestUninstallTitleTee_UserWrapperLeftAlone(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)
	t.Chdir(t.TempDir())

	settings := `{"title":{"type":"command","command":"my-wrapper.sh 'entire hooks antigravity title-tee'"}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["title"]; !ok {
		t.Error("user-authored wrapper containing the tee marker must be left untouched")
	}
}

// TestInstallTitle_RefusesSymlinkedSettingsFile: settings.json is a name inside
// agy's config directory, and a symlink at it is refused rather than followed
// or replaced. Following it would merge the target's contents into what Entire
// writes; the atomic rename would then silently swap the user's link for a
// regular file. Both happen without a word, so the legible answer is to stop.
func TestInstallTitle_RefusesSymlinkedSettingsFile(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	target := filepath.Join(t.TempDir(), "real-settings.json")
	if err := os.WriteFile(target, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cfgDir, "settings.json")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	if err := InstallTitleTee(); err == nil {
		t.Fatal("InstallTitleTee() error = nil, want refusal for a symlinked settings.json")
	}

	// The link is intact and its target untouched.
	info, err := os.Lstat(filepath.Join(cfgDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the user's symlink was replaced by a regular file")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"theme":"dark"}` {
		t.Fatalf("link target was modified: %s", got)
	}
	if TitleTeeInstalled() {
		t.Fatal("TitleTeeInstalled() = true through a symlink it must not read")
	}
}

// TestUninstallTitle_MissingConfigDirIsNoOp: a machine that never ran agy has
// no config directory at all, and uninstall must read that as nothing to do
// rather than as an error.
func TestUninstallTitle_MissingConfigDirIsNoOp(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	t.Setenv(configDirEnv, filepath.Join(t.TempDir(), "never-created"))

	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee() with no config dir: %v", err)
	}
	if TitleTeeInstalled() {
		t.Fatal("TitleTeeInstalled() = true with no config dir")
	}
}

// TestInstallTitle_SerialisesOnTheSettingsLock: install and uninstall are a
// read-modify-write of agy's machine-global settings.json, so two entire
// processes (an add in one repo racing an add or remove in another) must take
// turns. Pinned by holding the lock externally and watching install wait.
func TestInstallTitle_SerialisesOnTheSettingsLock(t *testing.T) {
	// No t.Parallel — uses t.Setenv
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)

	release, err := lockAgySettings(true)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- InstallTitleTee() }()

	select {
	case <-done:
		t.Fatal("InstallTitleTee completed while another process held the settings lock")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("InstallTitleTee after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("InstallTitleTee did not proceed after the lock was released")
	}
	if got, want := readTitleCommand(t, cfgDir), "entire hooks antigravity title-tee"; got != want {
		t.Fatalf("title.command = %q, want %q", got, want)
	}
}
