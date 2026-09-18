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

// newUserTierRepo builds a worktree with a project settings file and points
// the user config directory at a temp dir. It returns the project and local
// settings paths loadMergedSettings takes, and the worktree root.
//
// t.Setenv makes these tests process-global, so none of them call t.Parallel.
func newUserTierRepo(t *testing.T) (root, project, local string) {
	t.Helper()
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	project = filepath.Join(root, EntireSettingsFile)
	local = filepath.Join(root, EntireSettingsLocalFile)
	require.NoError(t, os.WriteFile(project, []byte(`{"enabled":true}`), 0o644))

	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Cleanup(ClearOriginKeyCache)
	return root, project, local
}

func writeUserSettings(t *testing.T, content string) {
	t.Helper()
	dir := os.Getenv(userdirs.EnvConfigDir)
	require.NotEmpty(t, dir, "newUserTierRepo must run first")
	require.NoError(t, os.WriteFile(filepath.Join(dir, usersettings.FileName), []byte(content), 0o600))
}

func TestUserTier_MachineWidePreferencesApply(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{"preferences": {"telemetry": true, "log_level": "debug", "review_fix_agent": "codex"}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	require.NotNil(t, s.Telemetry)
	assert.True(t, *s.Telemetry)
	assert.Equal(t, "debug", s.LogLevel)
	assert.Equal(t, "codex", s.ReviewFixAgent)
	assert.Empty(t, s.UserLayerRejections())
}

// A repos entry keyed by normalized origin applies only to the repository that
// origin names. This is the mechanism that lets one machine-wide file carry
// per-repository settings without a file inside any repository.
func TestUserTier_ReposEntryMatchesByOrigin(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	writeUserSettings(t, `{
	  "preferences": {"review_fix_agent": "claude-code"},
	  "repos": {
	    "github.com/acme/widgets": {"review_fix_agent": "codex"},
	    "github.com/other/thing":  {"review_fix_agent": "gemini"}
	  }
	}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "codex", s.ReviewFixAgent,
		"this repository's entry must win over the machine-wide default")
}

func TestUserTier_ReposEntryForAnotherRepoDoesNotApply(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	writeUserSettings(t, `{
	  "preferences": {"review_fix_agent": "claude-code"},
	  "repos": {"github.com/other/thing": {"review_fix_agent": "gemini"}}
	}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "claude-code", s.ReviewFixAgent,
		"a repos entry naming a different origin must not apply here")
}

// A repository with no usable origin keys by absolute worktree path, so the
// mechanism still reaches repositories that have no remote at all.
func TestUserTier_ReposEntryMatchesByPath(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)

	writeUserSettings(t, `{"repos": {`+jsonString(root)+`: {"review_fix_agent": "codex"}}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "codex", s.ReviewFixAgent)
}

// The allowlist is the boundary. An unknown key drops that block alone, with a
// reason recorded — it must never fail the load, because a mistyped review
// preference would then take the whole repository's settings down with it.
func TestUserTier_UnknownKeyDropsOnlyThatBlock(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{"preferences": {"external_agents": true, "telemetry": true}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err, "a bad preference block must not fail the load")

	assert.Nil(t, s.Telemetry, "the whole block is dropped, not merged in part")
	require.Len(t, s.UserLayerRejections(), 1)
	assert.Contains(t, s.UserLayerRejections()[0], "preferences")
	assert.True(t, s.Enabled, "the repository's own settings still applied")
}

// Precedence as shipped in this change: the user tier outranks the project
// file, and the per-worktree local file still outranks the user tier.
// Demoting the local file is a deliberate later step; pinning the current
// order here means that change cannot happen silently.
func TestUserTier_PrecedenceAgainstProjectAndLocal(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	require.NoError(t, os.WriteFile(project, []byte(`{"enabled":true,"review_fix_agent":"from-project"}`), 0o644))
	writeUserSettings(t, `{"preferences": {"review_fix_agent": "from-user"}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "from-user", s.ReviewFixAgent, "the user tier outranks the committed project file")

	require.NoError(t, os.WriteFile(local, []byte(`{"review_fix_agent":"from-local"}`), 0o644))
	s, err = loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "from-local", s.ReviewFixAgent,
		"the per-worktree local file still wins; demoting it is a separate change")
}

// The claim that a user file without a repos block costs no git reads. With
// git unreachable, a load carrying only machine-wide preferences must still
// succeed and apply them.
func TestUserTier_NoReposBlockNeedsNoGit(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{"preferences": {"review_fix_agent": "codex"}}`)
	t.Setenv("PATH", "")

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "codex", s.ReviewFixAgent,
		"machine-wide preferences must not depend on resolving a repository")
}

// A missing user settings file is an unconfigured tier, not a failure.
func TestUserTier_AbsentFileChangesNothing(t *testing.T) {
	_, project, local := newUserTierRepo(t)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.True(t, s.Enabled)
	assert.Empty(t, s.UserLayerRejections())
}

// jsonString quotes a path for embedding in a JSON object key.
func jsonString(s string) string {
	return `"` + filepath.ToSlash(s) + `"`
}
