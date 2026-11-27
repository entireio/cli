package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

func readUserSettings(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv(userdirs.EnvConfigDir), usersettings.FileName))
	require.NoError(t, err)
	return string(data)
}

// With no entry for the repository yet, the flag is written under its origin
// key, so it reaches every clone of it, and every other block is kept.
func TestSetRepoEnabled_CreatesAnOriginKeyedEntry(t *testing.T) {
	root := newNeverEnabledRepo(t)
	testutil.RunGit(t, root, "remote", "add", "origin", "git@github.com:Acme/Widgets.git")
	writeUserSettings(t, `{"global_enable": true, "newer_block": {"x": 1}}`)

	key, err := SetRepoEnabled(t.Context(), root, false)
	require.NoError(t, err)
	assert.Equal(t, "gh/acme/widgets", key)
	assert.JSONEq(t, `{
	  "global_enable": true,
	  "newer_block": {"x": 1},
	  "repos": {"gh/acme/widgets": {"enabled": false}}
	}`, readUserSettings(t))
}

// Every entry that names the repository is updated, so no lower-sorting entry
// can keep the old value in force; their other keys, and entries for other
// repositories, are kept.
func TestSetRepoEnabled_UpdatesEveryMatchingEntry(t *testing.T) {
	root := newNeverEnabledRepo(t)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	writeUserSettings(t, `{"repos": {
	  "gh/acme/widgets": {"log_level": "debug", "enabled": true},
	  `+jsonString(root)+`: {"enabled": true},
	  "gh/acme/other": {"enabled": true}
	}}`)

	_, err := SetRepoEnabled(t.Context(), root, false)
	require.NoError(t, err)
	assert.JSONEq(t, `{"repos": {
	  "gh/acme/widgets": {"log_level": "debug", "enabled": false},
	  `+jsonString(root)+`: {"enabled": false},
	  "gh/acme/other": {"enabled": true}
	}}`, readUserSettings(t))
}

// A repository with no origin is keyed by its main worktree's path, even when
// the flag is set from a linked worktree, so the entry reaches every worktree.
func TestSetRepoEnabled_KeysByMainWorktreeWithoutOrigin(t *testing.T) {
	main := newNeverEnabledRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(main, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, main, "add", ".")
	testutil.RunGit(t, main, "commit", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked-work", linked)

	key, err := SetRepoEnabled(t.Context(), linked, true)
	require.NoError(t, err)
	assert.True(t, usersettings.PathIsRoot(key, main), "key %q must name the main worktree", key)

	for _, tree := range []string{main, linked} {
		ClearOriginKeyCache()
		assert.True(t, UserTierConfiguresRepo(t.Context(), tree), "the entry must reach %s", filepath.Base(tree))
	}
}

// A repos block or matching entry that is not an object is refused, not
// replaced: the write would discard what the developer wrote there.
func TestSetRepoEnabled_RefusesANonObjectEntry(t *testing.T) {
	for name, content := range map[string]string{
		"repos not an object": `{"repos": [1]}`,
		"entry not an object": `{"repos": {"gh/acme/widgets": [1]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := newNeverEnabledRepo(t)
			testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
			writeUserSettings(t, content)

			_, err := SetRepoEnabled(t.Context(), root, true)
			require.Error(t, err)
			assert.Equal(t, content, readUserSettings(t))
		})
	}
}
