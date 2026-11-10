package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// `entire status` decided "not set up" from the two .entire files alone, which
// both live in the worktree. A repository configured through the user settings
// file has neither in a freshly added worktree, so status told the user their
// repository was not set up while its hooks were installed and running.
//
// The hook gate (IsSetUpAny) and this display path are separate, and fixing
// only the first left the surface a user actually looks at still wrong.
func TestStatus_ReportsEnabledWhenOnlyTheUserTierConfiguresTheRepo(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, root, "add", ".")
	testutil.RunGit(t, root, "commit", "-m", "init")

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	t.Chdir(root)

	require.NoDirExists(t, filepath.Join(root, ".entire"),
		"the premise: nothing configures this repository from inside the worktree")

	var before bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &before, false, false))
	assert.Contains(t, before.String(), "not set up",
		"sanity: with nothing configured anywhere, status says so")

	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName),
		[]byte(`{"repos":{"gh/acme/widgets":{"enabled":true}}}`), 0o600))
	settings.ClearOriginKeyCache()

	var after bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &after, false, false))
	assert.NotContains(t, after.String(), "not set up",
		"a repository configured in the user settings file is set up in every worktree")
	assert.Contains(t, after.String(), "Enabled")
}

// The JSON status path had the same worktree-only check as the text path, and
// fixing one call site and not the other left agents and scripts being told a
// running repository was not set up.
func TestStatusJSON_ReportsEnabledWhenOnlyTheUserTierConfiguresTheRepo(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, root, "add", ".")
	testutil.RunGit(t, root, "commit", "-m", "init")

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	t.Chdir(root)

	var before bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &before, false, true))
	assert.Contains(t, before.String(), "not set up", "sanity")

	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName),
		[]byte(`{"repos":{"gh/acme/widgets":{"enabled":true}}}`), 0o600))
	settings.ClearOriginKeyCache()

	var after bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &after, false, true))
	assert.NotContains(t, after.String(), "not set up",
		"the JSON path must agree with the text path about whether this repo is set up")
}

// A user settings file the CLI refuses to read (here a symlink) is otherwise
// ignored silently, so every setting in it looks like it is not working.
// Status says why.
func TestStatus_NamesARefusedUserSettingsFile(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	t.Chdir(root)

	target := filepath.Join(t.TempDir(), "elsewhere.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"global_enable":true}`), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(configDir, usersettings.FileName)))

	var out bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &out, false, false))
	assert.Contains(t, out.String(), "ignoring user settings")
	assert.Contains(t, out.String(), "symbolic link")
	assert.Contains(t, out.String(), "not set up", "the refused file configures nothing")
}

// Status names the committed keys a global-only repository ignores, so a team
// setting the developer can see in the file is not silently inert.
func TestStatus_NamesCommittedKeysIgnoredForGlobalEnable(t *testing.T) {
	setupTestRepo(t)
	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName), []byte(`{"global_enable": true}`), 0o600))
	writeSettings(t, `{"enabled": true, "telemetry": true, "strategy_options": {"summarize": {"enabled": true}}}`)

	for _, detailed := range []bool{false, true} {
		var out bytes.Buffer
		require.NoError(t, runStatus(t.Context(), &out, detailed, false))
		assert.Contains(t, out.String(), "ignoring strategy_options.summarize, telemetry from .entire/settings.json")
		assert.Contains(t, out.String(), "run 'entire enable' here to use them")
	}
}

// A part of the user settings file that was dropped is otherwise invisible:
// the user can see the setting in their own file and nothing says it is not in
// effect. Both status paths name it.
func TestStatus_NamesDroppedUserSettings(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	t.Chdir(root)

	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName), []byte(`{
	  "preferences": {"checkpoint_remote": {"provider": "github", "repo": "mydev/my-checkpoints"}},
	  "repos": {"gh/acme/widgets": {"enabled": true}}
	}`), 0o600))

	var text bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &text, false, false))
	assert.Contains(t, text.String(), "ignoring user setting: preferences: checkpoint_remote")

	var detailed bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &detailed, true, false))
	assert.Contains(t, detailed.String(), "ignoring user setting: preferences: checkpoint_remote")

	var out bytes.Buffer
	require.NoError(t, runStatus(t.Context(), &out, false, true))
	var got statusJSON
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Len(t, got.UserSettingsIgnored, 1)
	assert.Contains(t, got.UserSettingsIgnored[0], "checkpoint_remote")
}

// In a repository on only through global enable, status says whether its
// checkpoints push and why, in both status paths and in JSON.
func TestStatus_ReportsGlobalEnablePushes(t *testing.T) {
	for name, tc := range map[string]struct {
		file, want, wantState string
	}{
		"held":         {`{"global_enable": true}`, "checkpoint pushes held: 0 waiting", "held"},
		"owner listed": {`{"global_enable": true, "global_enable_owners": ["gh/acme"]}`, "gh/acme is on your global-enable owner list", "owner_listed"},
		"agreed":       {`{"global_enable": true, "repos": {"gh/acme/widgets": {"push_checkpoints": true}}}`, "you agreed for this repository", "agreed"},
	} {
		t.Run(name, func(t *testing.T) {
			root := setupTestRepo(t)
			testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
			configDir := t.TempDir()
			t.Setenv(userdirs.EnvConfigDir, configDir)
			settings.ClearOriginKeyCache()
			t.Cleanup(settings.ClearOriginKeyCache)
			require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName), []byte(tc.file), 0o600))

			for _, detailed := range []bool{false, true} {
				var out bytes.Buffer
				require.NoError(t, runStatus(t.Context(), &out, detailed, false))
				assert.Contains(t, out.String(), tc.want)
			}
			var out bytes.Buffer
			require.NoError(t, runStatus(t.Context(), &out, false, true))
			var got statusJSON
			require.NoError(t, json.Unmarshal(out.Bytes(), &got))
			require.NotNil(t, got.GlobalEnablePushes)
			assert.Equal(t, tc.wantState, got.GlobalEnablePushes.State)
		})
	}
}
