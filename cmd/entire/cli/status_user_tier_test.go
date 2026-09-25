package cli

import (
	"bytes"
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
