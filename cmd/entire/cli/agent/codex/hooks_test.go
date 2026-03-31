package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

func TestInstallHooks_CreatesConfig(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 3, count) // SessionStart, UserPromptSubmit, Stop

	// Verify hooks.json was created in the repo
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "entire hooks codex session-start")
	require.Contains(t, string(data), "entire hooks codex user-prompt-submit")
	require.Contains(t, string(data), "entire hooks codex stop")

	// Verify project-level config.toml enables codex_hooks feature (per-repo)
	projectConfig := filepath.Join(tempDir, ".codex", configFileName)
	projectData, err := os.ReadFile(projectConfig)
	require.NoError(t, err)
	require.Contains(t, string(projectData), "codex_hooks = true")
	require.Contains(t, string(projectData), "[features]")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	count1, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 3, count1)

	count2, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 0, count2)
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), true, false)
	require.NoError(t, err)
	require.Equal(t, 3, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `go run \"$(git rev-parse --show-toplevel)\"/cmd/entire/main.go hooks codex session-start`)
}

func TestInstallHooks_Force(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	count, err := ag.InstallHooks(context.Background(), false, true)
	require.NoError(t, err)
	require.Equal(t, 3, count)
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
