package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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
	claudeDir, stderr := runVersionWithUserFlag(t, `{"global_enable": true}`)

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
		"flag off":     `{"global_enable": false}`,
		"old key":      `{"preferences": {"enabled": true}}`,
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

// runGlobalCmd runs `entire <args>` in a fresh git repository with isolated
// Entire and Claude config directories, returning those directories and the
// command's output. claudeDir is created only when withClaude is set.
func runGlobalCmd(t *testing.T, withClaude bool, userSettings string, args ...string) (entireDir, claudeDir, repo, out string, err error) {
	t.Helper()
	entireDir = t.TempDir()
	claudeDir = filepath.Join(t.TempDir(), "claude")
	if withClaude {
		if mkErr := os.Mkdir(claudeDir, 0o700); mkErr != nil {
			t.Fatal(mkErr)
		}
	}
	repo = t.TempDir()
	testutil.InitRepo(t, repo)
	t.Setenv(userdirs.EnvConfigDir, entireDir)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Chdir(repo)
	if userSettings != "" {
		if wErr := os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(userSettings), 0o600); wErr != nil {
			t.Fatal(wErr)
		}
	}

	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err = root.Execute()
	return entireDir, claudeDir, repo, buf.String(), err
}

func readUserSettingsFile(t *testing.T, entireDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(entireDir, usersettings.FileName))
	if err != nil {
		t.Fatalf("read user settings: %v", err)
	}
	return string(data)
}

// `entire enable --global` writes the switch, keeps the rest of the file,
// installs Claude's user-level hooks at once, and leaves the repository it
// ran in untouched.
func TestEnableGlobal_WritesTheFlagAndInstallsHooks(t *testing.T) {
	entireDir, claudeDir, repo, out, err := runGlobalCmd(t, true,
		`{"preferences": {"telemetry": false}}`, "enable", "--global")
	if err != nil {
		t.Fatalf("enable --global: %v\n%s", err, out)
	}
	assert.JSONEq(t, `{"preferences": {"telemetry": false}, "global_enable": true}`, readUserSettingsFile(t, entireDir))

	hooks, readErr := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if readErr != nil || !strings.Contains(string(hooks), "entire hooks claude-code stop") {
		t.Errorf("Claude user-level hooks not installed (err=%v):\n%s", readErr, hooks)
	}
	if !strings.Contains(out, filepath.Join(claudeDir, "settings.json")) {
		t.Errorf("output does not name the hooks file:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".entire")); !os.IsNotExist(statErr) {
		t.Errorf("enable --global set up the current repository (stat err = %v)", statErr)
	}
}

// With no Claude config directory, nothing is created and the output does
// not claim hooks were installed.
func TestEnableGlobal_WithoutClaudeSaysSo(t *testing.T) {
	entireDir, claudeDir, _, out, err := runGlobalCmd(t, false, "", "enable", "--global")
	if err != nil {
		t.Fatalf("enable --global: %v\n%s", err, out)
	}
	assert.JSONEq(t, `{"global_enable": true}`, readUserSettingsFile(t, entireDir))
	if _, statErr := os.Stat(claudeDir); !os.IsNotExist(statErr) {
		t.Errorf("Claude config dir created (stat err = %v)", statErr)
	}
	if strings.Contains(out, "Added Entire's hooks") {
		t.Errorf("output claims hooks were installed:\n%s", out)
	}
}

func TestDisableGlobal_WritesFalseAndKeepsTheRest(t *testing.T) {
	entireDir, _, _, out, err := runGlobalCmd(t, true,
		`{"global_enable": true, "repos": {"github/acme/app": {"log_level": "debug"}}}`, "disable", "--global")
	if err != nil {
		t.Fatalf("disable --global: %v\n%s", err, out)
	}
	assert.JSONEq(t, `{"global_enable": false, "repos": {"github/acme/app": {"log_level": "debug"}}}`,
		readUserSettingsFile(t, entireDir))
}

// --global covers every repository, so a repository-scoped flag beside it is
// an error, and nothing is written.
func TestGlobal_RejectsRepositoryFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"enable --agent":      {"enable", "--global", "--agent", "claude-code"},
		"enable --local":      {"enable", "--global", "--local"},
		"enable --telemetry":  {"enable", "--global", "--telemetry=false"},
		"disable --project":   {"disable", "--global", "--project"},
		"disable --uninstall": {"disable", "--global", "--uninstall"},
	} {
		t.Run(name, func(t *testing.T) {
			entireDir, _, _, out, err := runGlobalCmd(t, true, "", args...)
			if err == nil {
				t.Fatalf("%v succeeded:\n%s", args, out)
			}
			if !strings.Contains(err.Error(), "--global") {
				t.Errorf("error does not name --global: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(entireDir, usersettings.FileName)); !os.IsNotExist(statErr) {
				t.Errorf("user settings written despite the error (stat err = %v)", statErr)
			}
		})
	}
}
