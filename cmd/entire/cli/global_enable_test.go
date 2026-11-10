package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	// The annotation decides, not the name.
	unannotated := &cobra.Command{Use: "enable"}
	(&cobra.Command{Use: "entire"}).AddCommand(unannotated)
	if skipsGlobalEnableHooks(unannotated) {
		t.Error("an unannotated command named enable skips the user-level hooks")
	}
}

// runGlobalCmd runs `entire <args>` in a fresh git repository with isolated
// Entire and Claude config directories, returning those directories and the
// command's output. claudeDir is created only when withClaude is set.
func runGlobalCmd(t *testing.T, withClaude bool, userSettings string, args ...string) (entireDir, claudeDir, repo, out string, err error) {
	t.Helper()
	return runGlobalCmdAs(t, withClaude, userSettings, "", args...)
}

// runGlobalCmdAs is runGlobalCmd with agentVar, when set, marking the caller
// as an agent session.
func runGlobalCmdAs(t *testing.T, withClaude bool, userSettings, agentVar string, args ...string) (entireDir, claudeDir, repo, out string, err error) {
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
	clearAgentCallerEnv(t)
	if agentVar != "" {
		t.Setenv(agentVar, "1")
	}
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
	assert.Contains(t, out, "records your Claude Code sessions in every git repository")
	assert.Contains(t, out, "other agents are recorded only where you ran it")
	assert.Contains(t, out, "stay on this machine until you agree")
}

// --owner adds to the owner list, normalized and without repeats, and the
// output lists every owner whose checkpoints now push without asking.
func TestEnableGlobal_OwnersAreAddedAndListed(t *testing.T) {
	entireDir, _, _, out, err := runGlobalCmd(t, true, `{"global_enable_owners": ["gh/acme"]}`,
		"enable", "--global", "--yes", "--owner", "GitHub.com/Other", "--owner", "gh/acme")
	if err != nil {
		t.Fatalf("enable --global --owner: %v\n%s", err, out)
	}
	assert.JSONEq(t, `{"global_enable": true, "global_enable_owners": ["gh/acme", "gh/other"]}`, readUserSettingsFile(t, entireDir))
	assert.Contains(t, out, "pushed with your git pushes without asking, including public repositories and forks they own")
	assert.Contains(t, out, "gh/acme, gh/other")
}

// An owner that is not one is refused before anything is written, and
// --owner means nothing without --global.
func TestEnableGlobal_RejectsBadOwnerUse(t *testing.T) {
	for name, args := range map[string][]string{
		"wildcard":       {"enable", "--global", "--yes", "--owner", "gh/*"},
		"subgroup":       {"enable", "--global", "--yes", "--owner", "gitlab.com/grp/sub"},
		"without global": {"enable", "--owner", "gh/acme"},
	} {
		t.Run(name, func(t *testing.T) {
			entireDir, _, _, out, err := runGlobalCmd(t, true, "", args...)
			if err == nil {
				t.Fatalf("%v succeeded:\n%s", args, out)
			}
			assert.NoFileExists(t, filepath.Join(entireDir, usersettings.FileName))
		})
	}
}

// Adding an owner while global enable is already on is something new to
// agree to; naming one already listed is not.
func TestEnableGlobal_NewOwnerNeedsConfirmation(t *testing.T) {
	const on = `{"global_enable": true, "global_enable_owners": ["gh/acme"]}`
	_, _, _, out, err := runGlobalCmd(t, true, on, "enable", "--global", "--owner", "gh/other")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("adding an owner without confirmation: err=%v\n%s", err, out)
	}
	_, _, _, out, err = runGlobalCmd(t, true, on, "enable", "--global", "--owner", "gh/acme")
	if err != nil {
		t.Fatalf("naming a listed owner: %v\n%s", err, out)
	}
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
	assert.NotContains(t, out, "repos entry", "no entry enables a repository")
}

// disable --global also takes Entire's hooks out of Claude Code's user-level
// settings, keeping the user's own keys, and names the repositories a repos
// entry alone enabled, which recorded only through those hooks.
func TestDisableGlobal_RemovesTheUserLevelClaudeHooks(t *testing.T) {
	const on = `{"global_enable": true, "repos": {"gh/acme/app": {"enabled": true}}}`
	_, claudeDir, _, out, err := runGlobalCmd(t, true, on, "enable", "--global")
	if err != nil {
		t.Fatalf("enable --global: %v\n%s", err, out)
	}
	hooksPath := filepath.Join(claudeDir, "settings.json")
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	var withUserKey map[string]any
	if err := json.Unmarshal(data, &withUserKey); err != nil {
		t.Fatal(err)
	}
	withUserKey["model"] = "opus"
	data, err = json.Marshal(withUserKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"disable", "--global"})
	if err := root.Execute(); err != nil {
		t.Fatalf("disable --global: %v\n%s", err, buf.String())
	}
	after := readJSONFile(t, hooksPath)
	assert.Equal(t, "opus", after["model"])
	assert.NotContains(t, readUserSettingsRaw(t, hooksPath), "entire hooks claude-code")
	assert.Contains(t, buf.String(), "Removed Entire's hooks from Claude Code's user settings")
	assert.Contains(t, buf.String(), "repos entry")
}

func readUserSettingsRaw(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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

// An agent session cannot turn global enable on, add an owner, or even
// re-run it once it is on: the answer is the user's. Nothing is written.
func TestEnableGlobal_RefusesAnAgentSession(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		args []string
	}{
		"turn on":      {"", []string{"enable", "--global", "--yes"}},
		"add an owner": {`{"global_enable": true}`, []string{"enable", "--global", "--yes", "--owner", "gh/acme"}},
		"already on":   {`{"global_enable": true}`, []string{"enable", "--global"}},
	} {
		t.Run(name, func(t *testing.T) {
			entireDir, claudeDir, _, out, err := runGlobalCmdAs(t, true, tc.file, "CLAUDECODE", tc.args...)
			if err == nil {
				t.Fatalf("%v from an agent session succeeded:\n%s", tc.args, out)
			}
			assert.Contains(t, err.Error(), "ask the user to run it")
			if tc.file == "" {
				assert.NoFileExists(t, filepath.Join(entireDir, usersettings.FileName))
			} else {
				assert.JSONEq(t, tc.file, readUserSettingsFile(t, entireDir))
			}
			assert.NoFileExists(t, filepath.Join(claudeDir, "settings.json"))
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
			assert.Contains(t, terminal.String(), "records your Claude Code sessions")
			assert.Contains(t, terminal.String(), "stay on this machine until you agree")
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
	clearAgentCallerEnv(t)
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

// userReposEntry returns the user settings file's repos[key], or nil.
func userReposEntry(t *testing.T, key string) map[string]any {
	t.Helper()
	repos, _ := readJSONFile(t, filepath.Join(os.Getenv(userdirs.EnvConfigDir), usersettings.FileName))["repos"].(map[string]any)
	entry, _ := repos[key].(map[string]any)
	return entry
}

// Running `entire enable` in a repository that was on only through global
// enable records that the developer enabled it themselves, whichever path
// enable takes: the bare already-enabled path, or recovering a repository
// whose committed settings had turned it off. The claim goes to the user
// settings file, so it covers every clone and worktree of the repository.
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
			testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
			if tc.committed != "" {
				writeSettings(t, tc.committed)
			}
			out, err := runRepoCmd(t, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			assert.Equal(t, true, userReposEntry(t, "gh/acme/widgets")["enabled"])
			assert.Contains(t, out, "every clone of gh/acme/widgets")
			if local, err := os.ReadFile(filepath.Join(repo, EntireSettingsLocalFile)); err == nil {
				assert.NotContains(t, string(local), `"enabled": true`, "the claim is clone-wide, not this worktree's")
			}

			settings.ClearOriginKeyCache()
			s, err := settings.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			assert.True(t, s.Enabled)
			assert.False(t, s.EnabledOnlyByGlobal())
		})
	}
}

// An agent cannot claim a repository on the developer's behalf: enable still
// succeeds, but the repository stays on only through global enable and the
// output says the developer has to run it themselves.
func TestEnable_AgentSessionDoesNotClaim(t *testing.T) {
	newGlobalOnlyRepo(t, true)
	t.Setenv("CLAUDECODE", "1")

	out, err := runRepoCmd(t, "enable")
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	assert.Contains(t, out, "ask the user to run `entire enable` here themselves")
	s, err := settings.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assert.True(t, s.EnabledOnlyByGlobal())
}

// The claim is best effort: enable already succeeded, so a failed write is a
// warning, not an error. A user settings file that does not parse is refused
// rather than rewritten.
func TestClaimRepoFromGlobalEnable_WarnsWhenTheWriteFails(t *testing.T) {
	newGlobalOnlyRepo(t, true)
	path := filepath.Join(os.Getenv(userdirs.EnvConfigDir), usersettings.FileName)
	if err := os.WriteFile(path, []byte(`{"global_enable": true, "repos": [1]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	claimRepoFromGlobalEnable(t.Context(), &out, &errOut)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Warning: Entire stays enabled here only through global enable")
}

// `entire disable` in a global-only repository turns it off for every clone
// and worktree through the user settings file, writes nothing in the
// worktree, and says global enable is still on elsewhere.
func TestDisable_GlobalOnlyRepo(t *testing.T) {
	repo := newGlobalOnlyRepo(t, true)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	out, err := runRepoCmd(t, "disable")
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	assert.Equal(t, false, userReposEntry(t, "gh/acme/widgets")["enabled"])
	assert.NoFileExists(t, filepath.Join(repo, EntireSettingsLocalFile))
	assert.Contains(t, out, "every clone of gh/acme/widgets")
	assert.Contains(t, out, "global enable stays on for your other repositories")

	settings.ClearOriginKeyCache()
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
