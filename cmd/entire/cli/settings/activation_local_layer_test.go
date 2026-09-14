package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// A committed .entire/settings.local.json is repository content: it must not
// activate a clone (nor bypass the user's exclusions), exactly as the merged
// loader ignores a tracked local layer. Not parallel: chdir + process caches.
func TestRepoActivation_IgnoresTrackedLocalLayer(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	local := filepath.Join(root, EntireSettingsLocalFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	require.NoError(t, os.WriteFile(local, []byte(`{"enabled":true}`), 0o644))
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	ClearVersionedPathCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		session.ClearGitCommonDirCache()
		ClearVersionedPathCache()
	})

	require.True(t, IsSetUpAndEnabled(t.Context()), "an untracked local file is the developer's own activation")

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "ship a local settings file")
	ClearVersionedPathCache()

	require.False(t, IsSetUpAndEnabled(t.Context()), "a tracked local file must not activate the repo")
	configured, err := RepoActivationConfigured(t.Context())
	require.NoError(t, err)
	require.False(t, configured, "a tracked local file is not repo-level configuration")
}
