package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// On a configured repository, enable's settings flags are saved like
// configure's, not dropped after the agent-management step.
func TestEnableCmd_ConfiguredRepoSavesGlobalSettingsFlags(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		check func(t *testing.T, s *settings.EntireSettings)
	}{
		"telemetry": {args: []string{"--telemetry=false"}, check: func(t *testing.T, s *settings.EntireSettings) {
			t.Helper()
			require.NotNil(t, s.Telemetry)
			require.False(t, *s.Telemetry)
		}},
		"absolute git hook path": {args: []string{"--absolute-git-hook-path"}, check: func(t *testing.T, s *settings.EntireSettings) {
			t.Helper()
			require.True(t, s.AbsoluteGitHookPath)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			setupTestRepo(t)
			writeSettings(t, testSettingsEnabled)
			writeClaudeHooksFixture(t)

			cmd := newEnableCmd()
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			require.NoError(t, cmd.Execute())

			s, err := settings.LoadFromFile(EntireSettingsFile)
			require.NoError(t, err)
			tc.check(t, s)
			require.Contains(t, stdout.String(), "Settings updated")
		})
	}
}

// On a disabled repository the settings write and the re-enable touch the
// same file; both must land.
func TestEnableCmd_DisabledRepoSavesTelemetryAndReenables(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--telemetry=false"})
	require.NoError(t, cmd.Execute())

	s, err := settings.LoadFromFile(EntireSettingsFile)
	require.NoError(t, err)
	require.True(t, s.Enabled)
	require.NotNil(t, s.Telemetry)
	require.False(t, *s.Telemetry)
}

// --local sends the setting to the local file and leaves the project's alone.
func TestEnableCmd_ConfiguredRepoSavesTelemetryLocally(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--local", "--telemetry=false"})
	require.NoError(t, cmd.Execute())

	local, err := settings.LoadFromFile(EntireSettingsLocalFile)
	require.NoError(t, err)
	require.NotNil(t, local.Telemetry)
	require.False(t, *local.Telemetry)
	project, err := settings.LoadFromFile(EntireSettingsFile)
	require.NoError(t, err)
	require.Nil(t, project.Telemetry, "the project file is unchanged")
}

// A --force reinstall alongside a settings flag follows the merged hook mode:
// absolute_git_hook_path set only in settings.local.json must survive
// `--force --telemetry=false` targeting settings.json, for enable and
// configure alike.
func TestForceTelemetryKeepsLocalAbsoluteHookPath(t *testing.T) {
	for name, newCmd := range map[string]func() *cobra.Command{"enable": newEnableCmd, "configure": newSetupCmd} {
		t.Run(name, func(t *testing.T) {
			setupTestRepo(t)
			writeSettings(t, testSettingsEnabled)
			writeLocalSettings(t, `{"enabled": true, "absolute_git_hook_path": true}`)
			writeClaudeHooksFixture(t)

			cmd := newCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--project", "--force", "--telemetry=false"})
			require.NoError(t, cmd.Execute())

			exe, err := os.Executable()
			require.NoError(t, err)
			resolved, err := filepath.EvalSymlinks(exe)
			require.NoError(t, err)
			hooksDir, err := strategy.GetHooksDir(context.Background())
			require.NoError(t, err)
			hook, err := os.ReadFile(filepath.Join(hooksDir, "post-commit"))
			require.NoError(t, err)
			require.Contains(t, string(hook), resolved, "the hook keeps the absolute binary path")
		})
	}
}
