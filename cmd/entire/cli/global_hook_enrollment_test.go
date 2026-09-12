package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/stretchr/testify/require"
)

func TestEnrollGlobalHookInstallationRequiresConfirmation(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	path, err := os.Executable()
	require.NoError(t, err)
	var out bytes.Buffer
	confirm := func(_ context.Context, prompt string, def bool) (bool, error) {
		require.Contains(t, prompt, path)
		require.False(t, def)
		return false, nil
	}
	require.NoError(t, enrollGlobalHookInstallation(t.Context(), &out, func() (string, error) { return path, nil }, confirm))
	_, err = globalhooks.Load()
	require.Error(t, err)
	confirm = func(context.Context, string, bool) (bool, error) { return true, nil }
	require.NoError(t, enrollGlobalHookInstallation(t.Context(), &out, func() (string, error) { return path, nil }, confirm))
	selected, err := globalhooks.Load()
	require.NoError(t, err)
	require.Equal(t, path, selected.Executable)
}

func TestSelectedInstallationSurvivesReconciliationAndScopeChanges(t *testing.T) {
	isolatedUserHome(t)
	pretendAgentBinaries(t, "claude", "gemini")
	writeUserSettings(t, `{"global":{"enabled":true}}`)
	selected := selectTestHookInstallation(t)
	var out bytes.Buffer
	globalPostRun(t.Context(), &out)
	got, err := globalhooks.Load()
	require.NoError(t, err)
	require.Equal(t, selected, got, "reconciliation from another binary must preserve selection")
	for _, enabled := range []bool{false, true} {
		require.NoError(t, settings.ModifyUserSettings(t.Context(), func(s *settings.UserSettings) error { s.Global.Enabled = enabled; return nil }))
		globalPostRun(t.Context(), &out)
		require.Equal(t, enabled, userHooksInstalled(t, string(agent.AgentNameClaudeCode)))
		require.Equal(t, enabled, userHooksInstalled(t, string(agent.AgentNameGemini)))
		got, err = globalhooks.Load()
		require.NoError(t, err)
		require.Equal(t, selected, got)
	}
	require.NoError(t, os.Remove(selected.Executable))
	out.Reset()
	globalPostRun(t.Context(), &out)
	require.Contains(t, out.String(), "need a usable selected installation")
	newPath := filepath.Join(t.TempDir(), "new-installation")
	require.NoError(t, os.WriteFile(newPath, []byte("replacement"), 0o700))
	executable := func() (string, error) { return newPath, nil }
	before, err := os.ReadFile(settings.UserSettingsPath())
	require.NoError(t, err)
	require.NoError(t, enrollGlobalHookInstallation(t.Context(), &out, executable, func(context.Context, string, bool) (bool, error) { return false, nil }))
	got, err = globalhooks.Load()
	require.Error(t, err)
	require.Equal(t, selected.Executable, got.Executable)
	require.NoError(t, enrollGlobalHookInstallation(t.Context(), &out, executable, func(context.Context, string, bool) (bool, error) { return true, nil }))
	got, err = globalhooks.Load()
	require.NoError(t, err)
	require.Equal(t, newPath, got.Executable)
	after, err := os.ReadFile(settings.UserSettingsPath())
	require.NoError(t, err)
	require.Equal(t, before, after, "enrollment must not change global activation settings")
	require.True(t, userHooksInstalled(t, string(agent.AgentNameClaudeCode)))
	require.True(t, userHooksInstalled(t, string(agent.AgentNameGemini)))
}

func TestReconcileWithoutSelectionRemovesOnlyManagedUserHooks(t *testing.T) {
	home := isolatedUserHome(t)
	writeUserSettings(t, `{"global":{"enabled":true}}`)
	claudeDir := filepath.Dir((&claudecode.ClaudeCodeAgent{}).HookConfigRelPath())
	geminiDir := filepath.Dir((&geminicli.GeminiCLIAgent{}).HookConfigRelPath())
	for _, dir := range []string{claudeDir, geminiDir} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, dir), 0o700))
		body := `{"model":"custom","hooks":{"Stop":[{"hooks":[{"type":"command","command":"entire hooks claude-code stop"},{"type":"command","command":"my-custom-hook"}]}]}}`
		if dir == geminiDir {
			body = `{"model":"custom","hooksConfig":{"enabled":true},"hooks":{"AfterAgent":[{"hooks":[{"name":"entire-after-agent","type":"command","command":"entire hooks gemini after-agent"},{"name":"mine","type":"command","command":"my-custom-hook"}]}]}}`
		}
		require.NoError(t, os.WriteFile(filepath.Join(home, dir, "settings.json"), []byte(body), 0o600))
	}
	var out bytes.Buffer
	globalPostRun(t.Context(), &out)
	for _, dir := range []string{claudeDir, geminiDir} {
		data, err := os.ReadFile(filepath.Join(home, dir, "settings.json"))
		require.NoError(t, err)
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &raw))
		require.Equal(t, `"custom"`, string(raw["model"]))
		require.Contains(t, string(data), "my-custom-hook")
		require.NotContains(t, string(data), "entire hooks")
	}
	_, err := globalhooks.Load()
	require.Error(t, err, "cleanup must not select an installation")
}
