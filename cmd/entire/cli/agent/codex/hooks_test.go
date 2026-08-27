package codex

import (
	"context"
	"encoding/json"
	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupTestEnv creates a temp dir, sets CWD and CODEX_HOME for test isolation.
// Cannot be parallel (uses t.Chdir and t.Setenv which are process-global).
func setupTestEnv(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, ".codex-home"))
	return tempDir
}

func TestInstallHooks_CreatesHooksJSONOnly(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	hooksFile, _ := readHooksFile(t, tempDir)

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.SessionEnd, agentpkg.WrapProductionSilentHookCommand("entire hooks codex session-end"), "SessionEnd")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")

	// Hooks are enabled by default in Codex, so no .codex/config.toml is
	// written. A TOML file there is actively harmful when the repo lives
	// inside <CODEX_HOME>/agents, where Codex's agent-role scanner rejects
	// it at startup (entireio/cli#842).
	projectConfig := filepath.Join(tempDir, ".codex", "config.toml")
	_, err = os.Stat(projectConfig)
	require.True(t, os.IsNotExist(err), "install must not create .codex/config.toml")
}

func TestInstallHooks_WindowsWrapperProbeSuccessKeepsWrappedCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", true)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	hooksFile, _ := readHooksFile(t, tempDir)

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.SessionEnd, agentpkg.WrapProductionSilentHookCommand("entire hooks codex session-end"), "SessionEnd")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
}

func TestInstallHooks_WindowsWrapperProbeFailureUsesWindowsCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", false)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	hooksFile, data := readHooksFile(t, tempDir)

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.SessionEnd, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex session-end"), "SessionEnd")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
	require.NotContains(t, string(data), "sh -c")
	require.NotContains(t, string(data), "command -v entire")
	require.Contains(t, string(data), "where.exe entire")
}

func TestInstallHooks_WindowsWrapperProbeFailureMigratesToWindowsCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	wrapperWorks := true
	withCodexHookEnvironmentFunc(t, "windows", func(context.Context, string) bool {
		return wrapperWorks
	})

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	wrapperWorks = false
	count, err = ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	hooksFile, data := readHooksFile(t, tempDir)

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.SessionEnd, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex session-end"), "SessionEnd")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
	require.NotContains(t, string(data), "sh -c")
	require.NotContains(t, string(data), "command -v entire")
	require.Contains(t, string(data), "where.exe entire")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	count1, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count1)

	count2, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, 0, count2)
}

func TestInstallHooks_RejectsOversizedHooksFile(t *testing.T) {
	tempDir := setupTestEnv(t)
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
	contents := `{"padding":"` + strings.Repeat("x", maxHooksFileBytes) + `"}`
	require.NoError(t, os.WriteFile(hooksPath, []byte(contents), 0o600))

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.ErrorContains(t, err, "exceeds 1048576 bytes")
}

func TestInstallAndUninstallHooks_RejectRedirectedTargets(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("symlink creation is not generally available on Windows")
	}
	tempDir := setupTestEnv(t)
	outside := t.TempDir()
	outsideHooks := filepath.Join(outside, HooksFileName)
	require.NoError(t, os.WriteFile(outsideHooks, []byte(`{"keep":true}`), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(tempDir, ".codex")))

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.Error(t, err)
	data, readErr := os.ReadFile(outsideHooks)
	require.NoError(t, readErr)
	require.Equal(t, `{"keep":true}`, string(data))

	require.Error(t, (&CodexAgent{}).UninstallHooks(context.Background()))
	data, readErr = os.ReadFile(outsideHooks)
	require.NoError(t, readErr)
	require.Equal(t, `{"keep":true}`, string(data))
}

func TestInstallAndUninstallHooks_RejectRedirectedHooksFile(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("symlink creation is not generally available on Windows")
	}
	tempDir := setupTestEnv(t)
	outside := t.TempDir()
	outsideHooks := filepath.Join(outside, HooksFileName)
	require.NoError(t, os.WriteFile(outsideHooks, []byte(`{"keep":true}`), 0o600))
	projectDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.Mkdir(projectDir, 0o750))
	require.NoError(t, os.Symlink(outsideHooks, filepath.Join(projectDir, HooksFileName)))

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.Error(t, err)
	require.Error(t, (&CodexAgent{}).UninstallHooks(context.Background()))
	data, readErr := os.ReadFile(outsideHooks)
	require.NoError(t, readErr)
	require.Equal(t, `{"keep":true}`, string(data))
}

func TestInstallHooks_ReplacesLegacyLocalDevHook(t *testing.T) {
	tempDir := setupTestEnv(t)
	ctx := context.Background()
	ag := &CodexAgent{}

	testutil.AssertLegacyHookReplaced(t,
		filepath.Join(tempDir, ".codex", HooksFileName),
		agentpkg.WrapProductionSilentHookCommandForOS("entire hooks codex stop", agentpkg.UseWindowsProductionHooks(ctx)),
		testutil.LegacyLocalDevCommand("hooks codex stop"),
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

func TestInstallHooks_Force(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	count, err := ag.InstallHooks(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)
}

// Codex clamps SessionEnd handlers to SESSION_END_MAX_TIMEOUT_SEC = 3 and
// prints "clamping SessionEnd hook timeout" at every startup when a config asks
// for more, so SessionEnd must be installed at exactly the ceiling while the
// between-turn hooks keep the standard timeout.
func TestInstallHooks_SessionEndUsesCodexTimeoutCeiling(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	hooksFile, _ := readHooksFile(t, tempDir)

	require.Equal(t, SessionEndTimeoutSec, entireHookTimeout(t, hooksFile.Hooks.SessionEnd, "SessionEnd"))
	require.Equal(t, defaultHookTimeoutSec, entireHookTimeout(t, hooksFile.Hooks.Stop, "Stop"))
}

// A SessionEnd hook left behind by an older Entire carries the 30s default,
// which makes Codex warn on every startup. Reinstalling must rewrite it rather
// than treat the command match alone as up to date.
func TestInstallHooks_RewritesSessionEndWithStaleTimeout(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	staleCommand := agentpkg.WrapProductionSilentHookCommand("entire hooks codex session-end")
	stale := HooksFile{Hooks: HookEvents{
		SessionEnd: []MatcherGroup{{
			Hooks: []HookEntry{{Type: "command", Command: staleCommand, Timeout: 30}},
		}},
	}}
	staleData, err := json.Marshal(stale)
	require.NoError(t, err)
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, staleData, 0o600))

	ag := &CodexAgent{}
	_, err = ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	hooksFile, _ := readHooksFile(t, tempDir)

	require.Equal(t, SessionEndTimeoutSec, entireHookTimeout(t, hooksFile.Hooks.SessionEnd, "SessionEnd"))
}

// readHooksFile reads and parses .codex/hooks.json under repoRoot, returning
// both the parsed form and the raw bytes (some assertions check the literal
// text, e.g. that no POSIX shell wrapper leaked into a Windows config).
func readHooksFile(t *testing.T, repoRoot string) (HooksFile, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, ".codex", HooksFileName))
	require.NoError(t, err)
	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))
	return hooksFile, data
}

// entireHookTimeout returns the timeout of the single Entire-managed hook in
// groups, failing if there is not exactly one.
func entireHookTimeout(t *testing.T, groups []MatcherGroup, label string) int {
	t.Helper()
	var timeouts []int
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if isEntireHook(hook.Command) {
				timeouts = append(timeouts, hook.Timeout)
			}
		}
	}
	require.Len(t, timeouts, 1, "%s should have exactly one Entire hook", label)
	return timeouts[0]
}

func TestUninstallHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	err = ag.UninstallHooks(context.Background())
	require.NoError(t, err)

	require.False(t, ag.AreHooksInstalled(context.Background()))
}

func TestUninstallHooks_PreservesUserHookContainingEntireSubstring(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"Stop": [
				{
					"matcher": null,
					"hooks": [
						{"type": "command", "command": "echo \"the entire workflow finished\""}
					]
				}
			]
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	err = ag.UninstallHooks(context.Background())
	require.NoError(t, err)

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.Contains(t, string(data), `echo \"the entire workflow finished\"`)
	require.NotContains(t, string(data), "entire hooks codex stop")
}

func TestAreHooksInstalled_NoFile(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}
	require.False(t, ag.AreHooksInstalled(context.Background()))
}

func TestAreHooksInstalled_WithHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	require.True(t, ag.AreHooksInstalled(context.Background()))
}

// TestAreHooksInstalled_PartialHooks — a single Entire hook is an installation,
// because UninstallHooks would strip it. Detection has to answer the question
// removal answers: the callers that skip removal on a false answer would
// otherwise leave this hook invoking an Entire no longer configured here.
//
// How complete the install is stays a separate question, and MissingEntireHooks
// is the one that answers it.
func TestAreHooksInstalled_PartialHooks(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(`{
		"hooks": {
			"Stop": [
				{
					"matcher": null,
					"hooks": [
						{"type": "command", "command": "entire hooks codex stop", "timeout": 30}
					]
				}
			]
		}
	}`), 0o600))

	ag := &CodexAgent{}
	require.True(t, ag.AreHooksInstalled(context.Background()))
	require.Equal(t, []string{
		"session_start", "session_end", "user_prompt_submit", "post_tool_use", "subagent_start", "subagent_stop",
	}, MissingEntireHooks(tempDir))
}

// TestAreHooksInstalled_PreSessionEndInstall — a user who enabled Codex before
// SessionEnd and the subagent hooks joined the install set still counts as
// installed, so Codex keeps
// appearing in `entire status` and the agent pickers instead of vanishing until
// they re-run enable. The gap is drift, and MissingEntireHooks reports it.
func TestAreHooksInstalled_PreSessionEndInstall(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(`{
		"hooks": {
			"SessionStart": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex session-start", "timeout": 30}]}],
			"UserPromptSubmit": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex user-prompt-submit", "timeout": 30}]}],
			"Stop": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex stop", "timeout": 30}]}],
			"PostToolUse": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex post-tool-use", "timeout": 30}]}]
		}
	}`), 0o600))

	ag := &CodexAgent{}
	require.True(t, ag.AreHooksInstalled(context.Background()))
	require.Equal(t, []string{"session_end", "subagent_start", "subagent_stop"}, MissingEntireHooks(tempDir))
}

func TestInstallHooks_PreservesExistingHooksJSON(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "^Bash$",
					"hooks": [
						{"type": "command", "command": "my-custom-hook"}
					]
				}
			]
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(existingConfig), 0o600))

	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(codexDir, HooksFileName))
	require.NoError(t, err)
	require.Contains(t, string(data), "my-custom-hook")
	require.Contains(t, string(data), "entire hooks codex stop")
}

func TestInstallHooks_ErrorsOnMalformedManagedHook(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"SessionStart": {"not": "an array"},
			"PreToolUse": [
				{
					"matcher": "^Bash$",
					"hooks": [
						{"type": "command", "command": "my-custom-hook"}
					]
				}
			]
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse SessionStart hooks")

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.JSONEq(t, existingConfig, string(data))
}

func TestUninstallHooks_ErrorsOnMalformedManagedHook(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"Stop": {"not": "an array"}
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	err := ag.UninstallHooks(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse Stop hooks")

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.JSONEq(t, existingConfig, string(data))
}

func TestInstallHooks_DoesNotModifyUserConfig(t *testing.T) {
	setupTestEnv(t)
	codexHome := os.Getenv("CODEX_HOME")

	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	existingConfig := "model = \"gpt-4.1\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	configData, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(configData), "model = \"gpt-4.1\"")
	require.NotContains(t, string(configData), `trust_level = "trusted"`)
}

// TestInstallHooks_LeavesExistingLocalConfigUntouched pins that install
// never reads, rewrites, or deletes a project-local .codex/config.toml —
// whether it's a user's own file or a feature-flag leftover from an older
// entire version. The CLI no longer manages that file at all; leftovers
// under <CODEX_HOME>/agents must be removed manually (entireio/cli#842).
func TestInstallHooks_LeavesExistingLocalConfigUntouched(t *testing.T) {
	contents := map[string]string{
		"old entire leftover": "[features]\nhooks = true\n",
		"user file":           "model = \"gpt-4.1\"\n",
	}
	for name, content := range contents {
		t.Run(name, func(t *testing.T) {
			tempDir := setupTestEnv(t)

			codexDir := filepath.Join(tempDir, ".codex")
			require.NoError(t, os.MkdirAll(codexDir, 0o750))
			configPath := filepath.Join(codexDir, "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o600))

			ag := &CodexAgent{}
			_, err := ag.InstallHooks(context.Background(), false)
			require.NoError(t, err)

			data, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.Equal(t, content, string(data), "install must not touch an existing .codex/config.toml")
		})
	}
}

// assertHookCommand verifies that one of the hook entries in groups contains the expected command.
func assertHookCommand(t *testing.T, groups []MatcherGroup, expectedCmd, label string) {
	t.Helper()
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Command == expectedCmd {
				return
			}
		}
	}
	t.Errorf("%s: expected hook command not found: %s", label, expectedCmd)
}

func withCodexHookEnvironment(t *testing.T, goos string, wrapperWorks bool) {
	t.Helper()
	withCodexHookEnvironmentFunc(t, goos, func(context.Context, string) bool {
		return wrapperWorks
	})
}

func withCodexHookEnvironmentFunc(t *testing.T, goos string, wrapperWorks func(context.Context, string) bool) {
	t.Helper()
	t.Cleanup(agentpkg.SetWindowsHookProbeForTesting(goos, wrapperWorks))
}

// TestInstallHooks_DropsLegacyHookAlongsideCurrent is the regression test for
// syncHookCommand returning early when the current command was already present,
// which left a legacy local-dev hook beside it so both fired.
func TestInstallHooks_DropsLegacyHookAlongsideCurrent(t *testing.T) {
	tempDir := setupTestEnv(t)
	ctx := context.Background()
	ag := &CodexAgent{}

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	current := agentpkg.WrapProductionSilentHookCommandForOS("entire hooks codex stop", agentpkg.UseWindowsProductionHooks(ctx))
	legacy := testutil.LegacyLocalDevCommand("hooks codex stop")

	testutil.AssertStaleHookDroppedAlongsideCurrent(t, hooksPath, current, legacy,
		func() {
			// Install, then append the legacy hook into the same Stop group.
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("seed InstallHooks() error = %v", err)
			}
			raw, err := os.ReadFile(hooksPath)
			require.NoError(t, err)
			var topLevel map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &topLevel))
			var rawHooks map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(topLevel["hooks"], &rawHooks))
			var stop []MatcherGroup
			require.NoError(t, parseHookType(rawHooks, "Stop", &stop))
			require.NotEmpty(t, stop)
			stop[0].Hooks = append(stop[0].Hooks, HookEntry{Type: "command", Command: legacy, Timeout: 30})
			marshalHookType(rawHooks, "Stop", stop)
			// Same marshaller InstallHooks uses: the production command contains
			// `>`, which encoding/json would escape to >.
			hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
			require.NoError(t, err)
			topLevel["hooks"] = hooksJSON
			out, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(hooksPath, out, 0o600))
		},
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

// TestCommittedDogfoodHooksIsCurrent guards this repo's own committed agent config against drifting from what
// InstallHooks writes. A stale committed config is how the pi extension ended up
// invoking a launcher script that had been deleted.
func TestCommittedDogfoodHooksIsCurrent(t *testing.T) {
	testutil.AssertCommittedDogfoodConfigStable(t, ".codex/hooks.json", func(t *testing.T, dir string) (int, error) {
		t.Helper()
		t.Chdir(dir)
		return (&CodexAgent{}).InstallHooks(context.Background(), false)
	})
}
