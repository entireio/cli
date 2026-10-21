package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// runVersionWithUserFlag runs `entire version` outside any repository with
// the given user settings file, and returns the isolated Claude config dir.
// t.Setenv/t.Chdir make these tests process-global, so no t.Parallel.
func runVersionWithUserFlag(t *testing.T, userSettings string) (claudeDir, stderr string) {
	t.Helper()
	entireDir := t.TempDir()
	claudeDir = t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, entireDir)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Chdir(t.TempDir())
	if userSettings != "" {
		if err := os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(userSettings), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	root := NewRootCmd()
	var errOut bytes.Buffer
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("entire version failed: %v", err)
	}
	return claudeDir, errOut.String()
}

// With the flag on, any Entire invocation — here one outside every
// repository — puts Entire's hooks into the user-level Claude settings, so
// Claude calls Entire in repositories that were never enabled.
func TestGlobalEnable_FlagOnInstallsUserLevelClaudeHooks(t *testing.T) {
	claudeDir, stderr := runVersionWithUserFlag(t, `{"preferences": {"enabled": true}}`)

	data, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatalf("user-level Claude settings not written: %v", err)
	}
	if !strings.Contains(string(data), "entire hooks claude-code stop") {
		t.Errorf("user-level Claude settings lack Entire's hooks:\n%s", data)
	}
	if !strings.Contains(stderr, filepath.Join(claudeDir, "settings.json")) {
		t.Errorf("stderr does not name the file it changed: %q", stderr)
	}
}

func TestGlobalEnable_FlagOffLeavesClaudeSettingsAlone(t *testing.T) {
	for name, file := range map[string]string{
		"no user file": "",
		"flag off":     `{"preferences": {"enabled": false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			claudeDir, _ := runVersionWithUserFlag(t, file)
			if _, err := os.Stat(filepath.Join(claudeDir, "settings.json")); !os.IsNotExist(err) {
				t.Errorf("user-level Claude settings touched with the flag off (stat err = %v)", err)
			}
		})
	}
}

// Hook invocations run every turn and `entire disable` removes Entire; neither
// (re)installs the user-level hooks.
func TestGlobalEnable_HookAndDisableCommandsSkipInstall(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	for _, args := range [][]string{
		{"hooks", "claude-code", "stop"},
		{"hooks", "git", "pre-push"},
		{"disable"},
	} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
		if !skipsGlobalEnableHooks(cmd) {
			t.Errorf("%v would install the user-level hooks", args)
		}
	}
	for _, args := range [][]string{{"version"}, {"status"}, {"enable"}} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
		if skipsGlobalEnableHooks(cmd) {
			t.Errorf("%v skips the user-level hooks", args)
		}
	}
}
