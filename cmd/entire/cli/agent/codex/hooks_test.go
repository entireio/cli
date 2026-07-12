package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
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

// setupReservedAgentsTestEnv creates a temp CODEX_HOME and puts the CWD
// (the "repo") inside <CODEX_HOME>/agents, reproducing the entireio/cli#842
// layout: a repo checked out under Codex's reserved custom-agent-role
// discovery tree. Returns (codexHome, repoRoot).
func setupReservedAgentsTestEnv(t *testing.T) (string, string) {
	t.Helper()
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, ".codex-home")
	repoRoot := filepath.Join(codexHome, "agents", "repos", "project")
	require.NoError(t, os.MkdirAll(repoRoot, 0o750))
	t.Chdir(repoRoot)
	t.Setenv("CODEX_HOME", codexHome)
	return codexHome, repoRoot
}

func TestInstallHooks_CreatesConfig(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count) // SessionStart, UserPromptSubmit, Stop, PostToolUse

	// Verify hooks.json was created in the repo
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")

	// Verify project-level config.toml enables the hooks feature (per-repo)
	projectConfig := filepath.Join(tempDir, ".codex", configFileName)
	projectData, err := os.ReadFile(projectConfig)
	require.NoError(t, err)
	require.Contains(t, string(projectData), "hooks = true")
	require.NotContains(t, string(projectData), "codex_hooks = true",
		"deprecated codex_hooks line must not be written by fresh installs")
	require.Contains(t, string(projectData), "[features]")
}

func TestInstallHooks_WindowsWrapperProbeSuccessKeepsWrappedCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", true)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
}

func TestInstallHooks_WindowsWrapperProbeFailureUsesWindowsCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", false)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
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
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	wrapperWorks = false
	count, err = ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
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

	count1, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count1)

	count2, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 0, count2)
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), true, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `\"$(git rev-parse --show-toplevel)\"/scripts/entire-dev hooks codex session-start`)
	require.Contains(t, string(data), `\"$(git rev-parse --show-toplevel)\"/scripts/entire-dev hooks codex post-tool-use`)
}

func TestInstallHooks_Force(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	count, err := ag.InstallHooks(context.Background(), false, true)
	require.NoError(t, err)
	require.Equal(t, 4, count)
}

func TestUninstallHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false, false)
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
	_, err := ag.InstallHooks(context.Background(), false, false)
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
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	require.True(t, ag.AreHooksInstalled(context.Background()))
}

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
	require.False(t, ag.AreHooksInstalled(context.Background()))
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

	_, err := ag.InstallHooks(context.Background(), false, false)
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
	_, err := ag.InstallHooks(context.Background(), false, false)
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
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, configFileName), []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	configData, err := os.ReadFile(filepath.Join(codexHome, configFileName))
	require.NoError(t, err)
	require.Contains(t, string(configData), "model = \"gpt-4.1\"")
	require.NotContains(t, string(configData), `trust_level = "trusted"`)
}

// TestInstallHooks_RewritesLegacyFeatureLine pins the rule that an existing
// `codex_hooks = true` line — written by older entire CLI versions — must
// be rewritten to the new `hooks = true` form on the next install. Codex
// 0.129.0 still accepts the legacy alias but prints a deprecation warning
// at every startup; rewriting silences it without forcing the user to
// touch their .codex/config.toml.
func TestInstallHooks_RewritesLegacyFeatureLine(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := "[features]\ncodex_hooks = true\n"
	configPath := filepath.Join(codexDir, configFileName)
	require.NoError(t, os.WriteFile(configPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	configData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(configData), "hooks = true")
	require.NotContains(t, string(configData), "codex_hooks = true",
		"legacy codex_hooks line must be replaced, not left alongside the new form")
}

// TestInstallHooks_ReservedAgentsDir_DoesNotWriteLocalConfigToml pins the
// entireio/cli#842 fix: when the repo lives under <CODEX_HOME>/agents,
// InstallHooks must not create a project-local .codex/config.toml, since
// Codex recursively scans that tree for agent-role TOML files and would
// misinterpret ours as a malformed role definition. hooks.json (JSON, not
// scanned) is unaffected.
func TestInstallHooks_ReservedAgentsDir_DoesNotWriteLocalConfigToml(t *testing.T) {
	codexHome, repoRoot := setupReservedAgentsTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	localConfig := filepath.Join(repoRoot, ".codex", configFileName)
	_, statErr := os.Stat(localConfig)
	require.True(t, os.IsNotExist(statErr), "reserved-tree install must not create a project-local .codex/config.toml")

	// hooks.json is JSON, not scanned by Codex's role-file discovery, so it
	// stays project-local as normal.
	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	_, err = os.Stat(hooksPath)
	require.NoError(t, err)

	// The feature flag is scoped to this project in the global config.toml.
	globalConfig := filepath.Join(codexHome, configFileName)
	data, err := os.ReadFile(globalConfig)
	require.NoError(t, err)
	require.Contains(t, string(data), scopedFeaturesHeader(repoRoot))
	require.Contains(t, string(data), featureLine)
}

// TestInstallHooks_ReservedAgentsDir_PreservesExistingGlobalConfig ensures
// the scoped write merges into an existing global config.toml rather than
// clobbering unrelated top-level settings or other projects' sections.
func TestInstallHooks_ReservedAgentsDir_PreservesExistingGlobalConfig(t *testing.T) {
	codexHome, repoRoot := setupReservedAgentsTestEnv(t)

	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	existing := "model = \"gpt-5.4\"\n\n" +
		"[projects.\"/some/other/repo\"]\n" +
		"trust_level = \"trusted\"\n\n" +
		"[projects.\"/some/other/repo\".features]\n" +
		"hooks = true\n"
	globalConfig := filepath.Join(codexHome, configFileName)
	require.NoError(t, os.WriteFile(globalConfig, []byte(existing), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	data, err := os.ReadFile(globalConfig)
	require.NoError(t, err)
	content := string(data)
	require.Contains(t, content, "model = \"gpt-5.4\"")
	require.Contains(t, content, "[projects.\"/some/other/repo\"]")
	require.Contains(t, content, "trust_level = \"trusted\"")
	require.Contains(t, content, scopedFeaturesHeader(repoRoot))
}

// TestInstallHooks_ReservedAgentsDir_Idempotent ensures re-enabling in a
// reserved-tree repo doesn't duplicate the scoped section.
func TestInstallHooks_ReservedAgentsDir_Idempotent(t *testing.T) {
	codexHome, repoRoot := setupReservedAgentsTestEnv(t)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	_, err = ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	globalConfig := filepath.Join(codexHome, configFileName)
	data, err := os.ReadFile(globalConfig)
	require.NoError(t, err)
	content := string(data)
	header := scopedFeaturesHeader(repoRoot)
	require.Equal(t, 1, strings.Count(content, header), "scoped section header must not be duplicated")
	require.Equal(t, 1, strings.Count(content, featureLine), "feature line must not be duplicated")
}

// TestInstallHooks_ReservedAgentsDir_CleansUpStaleLocalConfigToml pins the
// self-heal behavior: a project-local .codex/config.toml left behind by an
// older, buggy entire version (the exact file entireio/cli#842 reports) is
// removed once it contains only content this package manages.
func TestInstallHooks_ReservedAgentsDir_CleansUpStaleLocalConfigToml(t *testing.T) {
	_, repoRoot := setupReservedAgentsTestEnv(t)

	codexDir := filepath.Join(repoRoot, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	staleConfig := filepath.Join(codexDir, configFileName)
	require.NoError(t, os.WriteFile(staleConfig, []byte("[features]\nhooks = true\n"), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	_, statErr := os.Stat(staleConfig)
	require.True(t, os.IsNotExist(statErr), "stale entire-managed local config.toml must be removed")
}

// TestInstallHooks_ReservedAgentsDir_PreservesUnrelatedLocalConfigContent
// ensures the cleanup never deletes a local config.toml that carries
// content this package didn't write itself.
func TestInstallHooks_ReservedAgentsDir_PreservesUnrelatedLocalConfigContent(t *testing.T) {
	_, repoRoot := setupReservedAgentsTestEnv(t)

	codexDir := filepath.Join(repoRoot, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	staleConfig := filepath.Join(codexDir, configFileName)
	custom := "[features]\nhooks = true\nsome_other_setting = true\n"
	require.NoError(t, os.WriteFile(staleConfig, []byte(custom), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	data, err := os.ReadFile(staleConfig)
	require.NoError(t, err)
	require.Contains(t, string(data), "some_other_setting = true")
}

func TestIsUnderCodexAgentsDir(t *testing.T) {
	codexHome := "/home/user/.codex"
	require.True(t, isUnderCodexAgentsDir("/home/user/.codex/agents/repos/project", codexHome))
	require.True(t, isUnderCodexAgentsDir("/home/user/.codex/agents", codexHome))
	require.False(t, isUnderCodexAgentsDir("/home/user/code/project", codexHome))
	require.False(t, isUnderCodexAgentsDir("/home/user/.codex", codexHome))
	require.False(t, isUnderCodexAgentsDir("/home/user/.codex/agents-other/project", codexHome))
	// A directory literally named "..foo" really does live under agents/ and
	// must be detected as inside — a naive strings.HasPrefix(rel, "..") check
	// would wrongly treat it as an escape.
	require.True(t, isUnderCodexAgentsDir("/home/user/.codex/agents/..foo", codexHome))
	require.True(t, isUnderCodexAgentsDir("/home/user/.codex/agents/..foo/project", codexHome))
	// CODEX_HOME with a trailing slash resolves to the same reserved tree.
	require.True(t, isUnderCodexAgentsDir("/home/user/.codex/agents/repos/project", "/home/user/.codex/"))
}

// TestTomlQuoteString_EscapesForSafeTOMLKey pins the exact escaping used when
// embedding an attacker-influenceable repo path into the shared global
// config.toml. Any gap here is a TOML-injection vector: a path that can
// terminate the quoted key early could write arbitrary keys into the user's
// global Codex config.
func TestTomlQuoteString_EscapesForSafeTOMLKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `/plain/path`, `"/plain/path"`},
		{"quote", `a"b`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"newline", "a\nb", `"a\nb"`},
		{"tab", "a\tb", `"a\tb"`},
		{"carriage_return", "a\rb", `"a\rb"`},
		{"nul", "a\x00b", `"a\u0000b"`},
		{"del", "a\x7fb", `"a\u007Fb"`},
		{"vertical_tab", "a\x0bb", `"a\u000Bb"`},
		{"unicode", "héllo/你", `"héllo/你"`},
		// The classic injection attempt: close the quote and open a new key.
		{"injection_attempt", `x".hacked = "1`, `"x\".hacked = \"1"`},
	}
	for _, tc := range cases {
		got := tomlQuoteString(tc.in)
		require.Equal(t, tc.want, got, "case %s input %q", tc.name, tc.in)
		// Whatever the input, the rendered key must never contain a raw
		// newline or carriage return: that alone would split the header
		// across physical lines and break both parsing and idempotency.
		require.NotContains(t, got, "\n", "case %s produced a raw newline", tc.name)
		require.NotContains(t, got, "\r", "case %s produced a raw carriage return", tc.name)
	}
}

// TestScopedFeaturesHeader_IsAlwaysSingleLine guarantees the generated table
// header is a single physical line for pathological paths, which the
// line-based ensureLineInSection relies on for correct matching/idempotency.
func TestScopedFeaturesHeader_IsAlwaysSingleLine(t *testing.T) {
	for _, p := range []string{
		"/normal/repo",
		"a\nb\nc",
		"weird \"quoted\" .v2",
		"tab\tinside",
	} {
		header := scopedFeaturesHeader(p)
		require.Len(t, strings.Split(header, "\n"), 1, "header for %q must be one line: %q", p, header)
	}
}

// TestInstallHooks_ReservedAgentsDir_NewlineInPath_NoGlobalConfigInjection is
// the end-to-end proof of the no-injection property: a repo whose path
// contains a newline must not be able to smuggle an extra TOML line into the
// global config. Skips on filesystems that reject newline names.
func TestInstallHooks_ReservedAgentsDir_NewlineInPath_NoGlobalConfigInjection(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, ".codex-home")
	repoRoot := filepath.Join(codexHome, "agents", "repo\ninjected = \"evil\"")
	if err := os.MkdirAll(repoRoot, 0o750); err != nil {
		t.Skipf("filesystem rejects newline in directory name: %v", err)
	}
	t.Chdir(repoRoot)
	t.Setenv("CODEX_HOME", codexHome)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(codexHome, configFileName))
	require.NoError(t, err)
	content := string(data)

	// The header is present as a single escaped line...
	require.Contains(t, content, scopedFeaturesHeader(repoRoot))
	// ...and the newline in the path did NOT create a standalone physical
	// line resembling the injected assignment.
	for _, line := range strings.Split(content, "\n") {
		require.False(t, strings.HasPrefix(strings.TrimSpace(line), "injected"),
			"path newline injected a rogue TOML line: %q", line)
	}
}

// TestInstallHooks_ReservedAgentsDir_SpecialCharsPath_Idempotent runs the full
// install flow with a repo path containing a quote, a space and dots, then
// re-runs it, asserting the scoped section is written once and not duplicated.
func TestInstallHooks_ReservedAgentsDir_SpecialCharsPath_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, ".codex-home")
	repoRoot := filepath.Join(codexHome, "agents", `weird "dir".v2`, "proj")
	require.NoError(t, os.MkdirAll(repoRoot, 0o750))
	t.Chdir(repoRoot)
	t.Setenv("CODEX_HOME", codexHome)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	_, err = ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(codexHome, configFileName))
	require.NoError(t, err)
	content := string(data)
	header := scopedFeaturesHeader(repoRoot)
	require.Equal(t, 1, strings.Count(content, header), "scoped header must not be duplicated: %q", content)
}

// TestWriteScopedFeatureToGlobalConfig_ConcurrentReposMerge proves the global
// read-modify-write does not lose updates when two repos enable concurrently.
// Without the file lock, the second writer could clobber the first repo's
// section; the flock serializes them so both survive.
func TestWriteScopedFeatureToGlobalConfig_ConcurrentReposMerge(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, ".codex-home")
	repoA := filepath.Join(codexHome, "agents", "a")
	repoB := filepath.Join(codexHome, "agents", "b")
	require.NoError(t, os.MkdirAll(repoA, 0o750))
	require.NoError(t, os.MkdirAll(repoB, 0o750))

	repos := []string{repoA, repoB}
	errs := make([]error, len(repos))
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func(i int, r string) {
			defer wg.Done()
			errs[i] = ensureScopedProjectFeatureEnabled(codexHome, r)
		}(i, r)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	data, err := os.ReadFile(filepath.Join(codexHome, configFileName))
	require.NoError(t, err)
	content := string(data)
	require.Contains(t, content, scopedFeaturesHeader(repoA), "repo A section lost")
	require.Contains(t, content, scopedFeaturesHeader(repoB), "repo B section lost")
}

// TestCleanupStaleReservedTreeConfig_LineAnchored pins the self-heal safety
// boundary: a local config carrying only our managed lines is removed, but any
// file with a foreign line — even one that merely embeds our tokens as a
// substring, like `webhooks = true` — is preserved.
func TestCleanupStaleReservedTreeConfig_LineAnchored(t *testing.T) {
	// removedAfterClean writes body to a stale local config.toml, runs the
	// self-heal, and reports whether the file was removed.
	removedAfterClean := func(t *testing.T, body string) bool {
		t.Helper()
		dir := t.TempDir()
		codexDir := filepath.Join(dir, ".codex")
		require.NoError(t, os.MkdirAll(codexDir, 0o750))
		cfg := filepath.Join(codexDir, configFileName)
		require.NoError(t, os.WriteFile(cfg, []byte(body), 0o600))
		require.NoError(t, cleanupStaleReservedTreeConfig(dir))
		_, statErr := os.Stat(cfg)
		return os.IsNotExist(statErr)
	}

	// Only our content -> removed.
	require.True(t, removedAfterClean(t, "[features]\nhooks = true\n"),
		"pure entire-managed config must be removed")
	require.True(t, removedAfterClean(t, "[features]\ncodex_hooks = true\n"),
		"legacy entire-managed config must be removed")

	// Substring-only lookalikes and foreign keys -> preserved.
	require.False(t, removedAfterClean(t, "[features]\nwebhooks = true\n"),
		"`webhooks = true` is not our line and must be preserved")
	require.False(t, removedAfterClean(t, "[features]\nhooks = true\nsome_other_setting = true\n"),
		"config with a foreign setting must be preserved")
	require.False(t, removedAfterClean(t, "description = \"my [features] hooks = true list\"\n"),
		"a value embedding our tokens must be preserved")

	// A single malformed line concatenating our tokens is not something this
	// package ever writes; a substring strip would erase it to nothing and
	// delete the file, but a whole-line match keeps it.
	require.False(t, removedAfterClean(t, "[features]hooks = true\n"),
		"a concatenated non-managed line must be preserved")

	// A blank-only file was never clearly written by us -> preserved.
	require.False(t, removedAfterClean(t, "\n\n  \n"),
		"a blank file is not positively ours and must be preserved")
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
