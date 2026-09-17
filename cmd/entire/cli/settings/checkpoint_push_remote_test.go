package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func checkpointPushRemoteRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return root
}

func TestSetCheckpointPushRemoteLocal_CreatesLocalOverride(t *testing.T) {
	root := checkpointPushRemoteRepo(t)
	changed, err := SetCheckpointPushRemoteLocal(t.Context(), "private")
	require.NoError(t, err)
	require.True(t, changed)
	data, err := os.ReadFile(filepath.Join(root, EntireSettingsLocalFile))
	require.NoError(t, err)
	require.JSONEq(t, `{"strategy_options":{"checkpoint_push_remote":"private"}}`, string(data))
	_, err = os.Stat(filepath.Join(root, EntireSettingsFile))
	require.True(t, os.IsNotExist(err))
	loaded, err := Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "private", loaded.GetCheckpointPushRemote())
}

func TestSetCheckpointPushRemoteLocal_PreservesUnrelatedSettings(t *testing.T) {
	root := checkpointPushRemoteRepo(t)
	project := `{"enabled":false,"strategy_options":{"checkpoint_push_remote":"private","shared_only":true}}`
	testutil.WriteFile(t, root, EntireSettingsFile, project)
	testutil.WriteFile(t, root, EntireSettingsLocalFile, `{"log_level":"DEBUG","strategy_options":{"future":{"value":9007199254740993},"checkpoint_remote":{"provider":"github","repo":"me/archive"},"auto_push":false,"unknown":[1,2]}}`)
	changed, err := SetCheckpointPushRemoteLocal(t.Context(), "private")
	require.NoError(t, err)
	require.True(t, changed, "an explicit local override is required even when the shared value matches")
	data, err := os.ReadFile(filepath.Join(root, EntireSettingsFile))
	require.NoError(t, err)
	require.Equal(t, project, string(data))
	data, err = os.ReadFile(filepath.Join(root, EntireSettingsLocalFile))
	require.NoError(t, err)
	require.JSONEq(t, `{"log_level":"DEBUG","strategy_options":{"future":{"value":9007199254740993},"checkpoint_push_remote":"private","checkpoint_remote":{"provider":"github","repo":"me/archive"},"auto_push":false,"unknown":[1,2]}}`, string(data))
	require.Contains(t, string(data), "9007199254740993")
	loaded, err := Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "private", loaded.GetCheckpointPushRemote())
	require.False(t, loaded.Enabled)
}

func TestSetCheckpointPushRemoteLocal_Idempotent(t *testing.T) {
	root := checkpointPushRemoteRepo(t)
	body := `{"strategy_options": {"checkpoint_push_remote": "private"}}`
	testutil.WriteFile(t, root, EntireSettingsLocalFile, body)
	path := filepath.Join(root, EntireSettingsLocalFile)
	stamp := time.Unix(1000000000, 0)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
	changed, err := SetCheckpointPushRemoteLocal(t.Context(), "private")
	require.NoError(t, err)
	require.False(t, changed)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, body, string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, stamp, info.ModTime())
}

func TestSetCheckpointPushRemoteLocal_RejectsInvalidSettings(t *testing.T) {
	for _, body := range []string{`null`, `{"strategy_options":null}`, `{"strategy_options":[]}`, `{"strategy_options":"wrong"}`, `{"strategy_options":`, `{"strategy_options":{"checkpoint_push_remote":3}}`} {
		t.Run(body, func(t *testing.T) {
			root := checkpointPushRemoteRepo(t)
			testutil.WriteFile(t, root, EntireSettingsLocalFile, body)
			changed, err := SetCheckpointPushRemoteLocal(t.Context(), "private")
			require.Error(t, err)
			require.False(t, changed)
			data, err := os.ReadFile(filepath.Join(root, EntireSettingsLocalFile))
			require.NoError(t, err)
			require.Equal(t, body, string(data))
		})
	}
}

func TestSetCheckpointPushRemoteLocal_RejectsEmptyName(t *testing.T) {
	root := checkpointPushRemoteRepo(t)
	changed, err := SetCheckpointPushRemoteLocal(t.Context(), "")
	require.Error(t, err)
	require.False(t, changed)
	_, err = os.Stat(filepath.Join(root, EntireSettingsLocalFile))
	require.True(t, os.IsNotExist(err))
}

func TestSetCheckpointPushRemoteLocal_RejectsTrackedLocalLayer(t *testing.T) {
	root := checkpointPushRemoteRepo(t)
	body := `{"strategy_options":{"checkpoint_push_remote":"private"}}`
	testutil.WriteFile(t, root, EntireSettingsLocalFile, body)
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	changed, err := SetCheckpointPushRemoteLocal(t.Context(), "private")
	require.ErrorContains(t, err, "tracked")
	require.False(t, changed)
	data, err := os.ReadFile(filepath.Join(root, EntireSettingsLocalFile))
	require.NoError(t, err)
	require.Equal(t, body, string(data))
}

func TestSetCheckpointPushRemoteLocal_RejectsSymlink(t *testing.T) {
	root := checkpointPushRemoteRepo(t)
	testutil.WriteFile(t, root, EntireSettingsFile, `{}`)
	target := filepath.Join(t.TempDir(), "outside.json")
	writeSettingsFile(t, target, `{}`)
	if err := os.Symlink(target, filepath.Join(root, EntireSettingsLocalFile)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	changed, err := SetCheckpointPushRemoteLocal(t.Context(), "private")
	require.ErrorIs(t, err, paths.ErrEntireDirUnsupportedEntry)
	require.False(t, changed)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, `{}`, string(data))
}
