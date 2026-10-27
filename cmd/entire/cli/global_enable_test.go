package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
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

// Hidden commands (hook invocations, background workers), `entire enable`
// (which installs hooks itself) and `entire disable` do not (re)install the
// user-level hooks.
func TestGlobalEnable_HiddenEnableAndDisableCommandsSkipInstall(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	for _, args := range [][]string{
		{"hooks", "claude-code", "stop"},
		{"hooks", "git", "pre-push"},
		{"__sweep_sessions"},
		{"enable"},
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
	for _, args := range [][]string{{"version"}, {"status"}} {
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
		`{"preferences": {"telemetry": false}}`, "enable", "--global", "--yes")
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
	assert.Contains(t, out, "Accepted with --yes")
	assert.Contains(t, out, "public repositories and forks")
}

// With no Claude config directory, nothing is created and the output does
// not claim hooks were installed.
func TestEnableGlobal_WithoutClaudeSaysSo(t *testing.T) {
	entireDir, claudeDir, _, out, err := runGlobalCmd(t, false, "", "enable", "--global", "-y")
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
		"enable --telemetry":  {"enable", "--global", "--yes", "--telemetry=false"},
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

// Without a terminal to ask in and without --yes, nothing is written: not the
// user settings file, and not Claude's hooks.
func TestEnableGlobal_RefusesWithoutConfirmation(t *testing.T) {
	entireDir, claudeDir, _, out, err := runGlobalCmd(t, true, "", "enable", "--global")
	if err == nil {
		t.Fatalf("enable --global without a terminal or --yes succeeded:\n%s", out)
	}
	assert.Contains(t, err.Error(), "--yes")
	assert.NoFileExists(t, filepath.Join(entireDir, usersettings.FileName))
	assert.NoFileExists(t, filepath.Join(claudeDir, "settings.json"))
}

// Running it again once it is on asks nothing: there is nothing new to agree to.
func TestEnableGlobal_AlreadyOnNeedsNoConfirmation(t *testing.T) {
	_, _, _, out, err := runGlobalCmd(t, true, `{"global_enable": true}`, "enable", "--global")
	if err != nil {
		t.Fatalf("enable --global with the flag already on: %v\n%s", err, out)
	}
}

// The prompt shows the disclosure where the question is asked; declining
// writes nothing, accepting writes the flag.
func TestEnableGlobal_PromptAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		want   bool
	}{
		"accept":  {"y\n", true},
		"decline": {"n\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(interactive.EnvTestTTY, "1")
			t.Setenv("ACCESSIBLE", "1")
			var terminal bytes.Buffer
			prev := openPromptTerminal
			openPromptTerminal = func() (promptTerminal, error) {
				return promptTerminal{in: strings.NewReader(tc.answer), out: &terminal}, nil
			}
			t.Cleanup(func() { openPromptTerminal = prev })

			entireDir, _, _, out, err := runGlobalCmd(t, false, "", "enable", "--global")
			if err != nil {
				t.Fatalf("enable --global: %v\n%s", err, out)
			}
			assert.Contains(t, terminal.String(), "public repositories and forks")
			if tc.want {
				assert.JSONEq(t, `{"global_enable": true}`, readUserSettingsFile(t, entireDir))
			} else {
				assert.NoFileExists(t, filepath.Join(entireDir, usersettings.FileName))
			}
		})
	}
}

// newGlobalOnlyRepo is a git repository with no settings of its own, in a
// process whose user settings turn global enable on (or off).
func newGlobalOnlyRepo(t *testing.T, globalEnable bool) string {
	t.Helper()
	repo := setupTestRepo(t)
	entireDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, entireDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Cleanup(settings.ClearOriginKeyCache)
	content := `{"global_enable": false}`
	if globalEnable {
		content = `{"global_enable": true}`
	}
	if err := os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

// runRepoCmd runs `entire enable`/`entire disable` (args[0]) in the current
// repository. The identity resolver fails: these repositories already have a
// git identity, so asking for one is a bug.
func runRepoCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var cmd *cobra.Command
	if args[0] == "disable" {
		cmd = newDisableCmd()
	} else {
		cmd = newEnableCmdWithIdentityResolverFactory(func(io.Writer, io.Writer, bool) gitIdentityResolver {
			return func(context.Context) (*authProfile, error) {
				return nil, errors.New("identity resolver called")
			}
		})
	}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args[1:])
	err := cmd.Execute()
	return buf.String(), err
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// Running `entire enable` in a repository that was on only through global
// enable records that the developer enabled it themselves, whichever path
// enable takes: the bare already-enabled path, --agent, or recovering a
// repository whose committed settings had turned it off.
func TestEnable_ClaimsARepoEnabledOnlyByGlobal(t *testing.T) {
	for name, tc := range map[string]struct {
		committed string
		args      []string
	}{
		"already enabled":    {args: []string{"enable"}},
		"committed disabled": {committed: `{"enabled": false}`, args: []string{"enable"}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newGlobalOnlyRepo(t, true)
			if tc.committed != "" {
				writeSettings(t, tc.committed)
			}
			out, err := runRepoCmd(t, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			assert.Equal(t, true, readJSONFile(t, filepath.Join(repo, EntireSettingsLocalFile))["enabled"])
			assert.Contains(t, out, "not only through global enable")

			s, err := settings.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			assert.True(t, s.Enabled)
			assert.False(t, s.EnabledOnlyByGlobal())
		})
	}
}

// A tracked local file is not this developer's own, so enable does not write
// the claim into it; it says why instead.
func TestEnable_DoesNotClaimThroughATrackedLocalFile(t *testing.T) {
	repo := newGlobalOnlyRepo(t, true)
	if err := os.MkdirAll(filepath.Join(repo, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(repo, EntireSettingsLocalFile)
	if err := os.WriteFile(local, []byte(`{"commit_linking": "always"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "add", "-f", EntireSettingsLocalFile)

	out, err := runRepoCmd(t, "enable")
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	_, hasEnabled := readJSONFile(t, local)["enabled"]
	assert.False(t, hasEnabled, "the claim must not be written into a tracked file")
	assert.Contains(t, out, "tracked in git")
}

// The claim is best effort: enable already succeeded, so a failed write is a
// warning, not an error.
func TestClaimRepoFromGlobalEnable_WarnsWhenTheWriteFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block writes on Windows")
	}
	repo := newGlobalOnlyRepo(t, true)
	entire := filepath.Join(repo, ".entire")
	if err := os.MkdirAll(entire, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(entire, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(entire, 0o755); err != nil {
			t.Error(err)
		}
	})

	var out, errOut bytes.Buffer
	claimRepoFromGlobalEnable(t.Context(), &out, &errOut)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Warning: Entire stays enabled here only through global enable")
}

// `entire disable` in a global-only repository writes the local file, keeps
// it out of git, and says global enable is still on elsewhere.
func TestDisable_GlobalOnlyRepo(t *testing.T) {
	repo := newGlobalOnlyRepo(t, true)
	out, err := runRepoCmd(t, "disable")
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	assert.Equal(t, false, readJSONFile(t, filepath.Join(repo, EntireSettingsLocalFile))["enabled"])
	gitignore, readErr := os.ReadFile(filepath.Join(repo, ".entire", ".gitignore"))
	if readErr != nil {
		t.Fatalf("disable did not keep settings.local.json out of git: %v", readErr)
	}
	assert.Contains(t, string(gitignore), "settings.local.json")
	assert.Contains(t, out, "global enable stays on for your other repositories")

	s, err := settings.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assert.False(t, s.Enabled)
}

// `entire disable --project` in a global-only repository writes the committed
// file and still keeps the local file out of git, but does not print the
// "stays on elsewhere" line: that line is only for the local-file path.
func TestDisableProject_GlobalOnlyRepo(t *testing.T) {
	repo := newGlobalOnlyRepo(t, true)
	out, err := runRepoCmd(t, "disable", "--project")
	if err != nil {
		t.Fatalf("disable --project: %v\n%s", err, out)
	}
	assert.Equal(t, false, readJSONFile(t, filepath.Join(repo, EntireSettingsFile))["enabled"])
	assert.NoFileExists(t, filepath.Join(repo, EntireSettingsLocalFile))
	gitignore, readErr := os.ReadFile(filepath.Join(repo, ".entire", ".gitignore"))
	if readErr != nil {
		t.Fatalf("disable --project did not write .entire/.gitignore: %v", readErr)
	}
	assert.Contains(t, string(gitignore), "settings.local.json")
	assert.NotContains(t, out, "global enable stays on for your other repositories")

	s, err := settings.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assert.False(t, s.Enabled)
}

// With global enable off, disable is exactly what it was: no .gitignore, and
// no mention of global enable.
func TestDisable_GlobalOffIsUnchanged(t *testing.T) {
	repo := newGlobalOnlyRepo(t, false)
	writeSettings(t, `{"enabled": true}`)
	out, err := runRepoCmd(t, "disable")
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	assert.NoFileExists(t, filepath.Join(repo, ".entire", ".gitignore"))
	assert.NotContains(t, out, "global enable")
}

// The SessionStart banner follows the user flag, which it checks before
// spawning git to look for hooks.
func TestGlobalEnableBannerSuffix_FollowsTheUserFlag(t *testing.T) {
	for _, on := range []bool{true, false} {
		newGlobalOnlyRepo(t, on)
		got := globalEnableBannerSuffix(t.Context(), agent.AgentNameClaudeCode)
		if on != strings.Contains(got, "through global enable") {
			t.Errorf("global_enable %v: banner suffix %q", on, got)
		}
	}
}
