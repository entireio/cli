package remote

import (
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

// A checkpoint destination whose owner differs from origin's is refused as
// inherited unless the developer vouches for it. The user settings file is a
// place a repository cannot write, so a destination found there is the
// developer's own by construction and needs no probe.
//
// This exists because the helper that answers it was written and left
// unwired: the value loaded into settings and was then refused as inherited,
// which a user experiences as the setting being ignored for no reason. Unit
// tests of the loader all passed; only an end-to-end run caught it.
func TestCheckpointRemoteIsInherited_UserTierProvesOwnership(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/upstream/app.git")
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, root, "add", ".")
	testutil.RunGit(t, root, "commit", "-m", "init")

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	t.Chdir(root)

	config := &settings.CheckpointRemoteConfig{Provider: "github", Repo: "mydev/my-checkpoints"}
	const originURL = "https://github.com/upstream/app.git"

	verdict, reason := checkpointRemoteIsInherited(t.Context(), config, originURL, nil)
	assert.True(t, verdict.Refused(),
		"sanity: a differently-owned destination with nothing vouching for it is inherited")
	assert.Contains(t, reason, "upstream")

	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName), []byte(
		`{"repos":{"gh/upstream/app":{"checkpoint_remote":{"provider":"github","repo":"mydev/my-checkpoints"}}}}`), 0o600))
	settings.ClearOriginKeyCache()

	verdict, _ = checkpointRemoteIsInherited(t.Context(), config, originURL, nil)
	assert.Equal(t, OwnershipOurs, verdict,
		"a destination named in the user settings file is the developer's own, in every worktree")
}

// The user tier vouches only for the destination it supplied. When a later
// layer overrides it with a different destination, that one still needs its
// own proof: here a committed .entire/settings.local.json names a store owned
// by someone else, and the user tier naming a different store must not wave it
// through.
func TestCheckpointRemoteIsInherited_UserTierDoesNotVouchForAnOverride(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/upstream/app.git")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".entire", "settings.local.json"), []byte(
		`{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"stranger/their-checkpoints"}}}`), 0o644))
	testutil.RunGit(t, root, "add", "-f", ".")
	testutil.RunGit(t, root, "commit", "-m", "init")

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(settings.ClearOriginKeyCache)
	t.Chdir(root)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName), []byte(
		`{"repos":{"gh/upstream/app":{"checkpoint_remote":{"provider":"github","repo":"mydev/my-checkpoints"}}}}`), 0o600))
	settings.ClearOriginKeyCache()

	const originURL = "https://github.com/upstream/app.git"
	override := &settings.CheckpointRemoteConfig{Provider: "github", Repo: "stranger/their-checkpoints"}
	verdict, reason := checkpointRemoteIsInherited(t.Context(), override, originURL, nil)
	assert.True(t, verdict.Refused(), "a destination the user tier did not supply is not vouched for by it")
	assert.Contains(t, reason, "upstream")

	own := &settings.CheckpointRemoteConfig{Provider: "github", Repo: "mydev/my-checkpoints"}
	verdict, _ = checkpointRemoteIsInherited(t.Context(), own, originURL, nil)
	assert.Equal(t, OwnershipOurs, verdict, "the user tier's own destination is still the developer's")
}
