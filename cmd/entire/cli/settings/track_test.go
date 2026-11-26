package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestNormalizeTrackOrg(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"github.com/entireio":          "gh/entireio",
		"https://github.com/EntireIO/": "gh/entireio",
		"gh/entireio":                  "gh/entireio",
		"gitlab.example.com/team":      "gitlab.example.com/team",
		"github.com":                   "",
		"":                             "",
	} {
		assert.Equal(t, want, NormalizeTrackOrg(in), in)
	}
}

func TestNormalizeTrackRepo(t *testing.T) {
	t.Parallel()
	key, err := NormalizeTrackRepo("git@github.com:Acme/Widgets.git")
	require.NoError(t, err)
	assert.Equal(t, "gh/acme/widgets", key)
	key, err = NormalizeTrackRepo("github.com/acme/widgets")
	require.NoError(t, err)
	assert.Equal(t, "gh/acme/widgets", key)
	key, err = NormalizeTrackRepo("/src/thing")
	require.NoError(t, err)
	assert.Equal(t, "/src/thing", key)
	_, err = NormalizeTrackRepo("nonsense")
	require.Error(t, err)
}

// newTrackRepo is a never-enabled repository with origin (none when empty)
// and isolated user settings.
func newTrackRepo(t *testing.T, origin string) string {
	t.Helper()
	root := newNeverEnabledRepo(t)
	if origin != "" {
		testutil.RunGit(t, root, "remote", "add", "origin", origin)
	}
	t.Cleanup(ClearTrackCache)
	ClearTrackCache()
	return root
}

func trackable(t *testing.T, root string) (bool, string) {
	t.Helper()
	ClearTrackCache()
	return RepoTrackable(t.Context(), root)
}

// The precedence table, first match wins, across origin shapes.
func TestRepoTrackable_Precedence(t *testing.T) {
	for name, tc := range map[string]struct {
		origin, user, committed, local string
		want                           bool
		reason                         string
	}{
		"no rule tracks everything": {
			origin: "git@github.com:other/x.git", user: `{"global_enable": true}`, want: true, reason: "every repository",
		},
		"committed enabled false wins over an explicit allow": {
			origin: "https://github.com/entireio/cli.git", committed: `{"enabled": false}`,
			user: `{"track": {"repos": ["gh/entireio/cli"]}}`, reason: "disabled by",
		},
		"repos enabled false wins over the org": {
			origin: "https://github.com/entireio/cli.git",
			user:   `{"repos": {"gh/entireio/cli": {"enabled": false}}, "track": {"orgs": ["github.com/entireio"]}}`, reason: "repos",
		},
		"deny wins over the org and an explicit allow": {
			origin: "git@github.com:entireio/cli.git",
			user:   `{"track": {"orgs": ["github.com/entireio"], "repos": ["gh/entireio/cli"], "deny": ["github.com/entireio/cli"]}}`, reason: "track.deny",
		},
		"ssh origin in the org": {
			origin: "git@github.com:EntireIO/cli.git", user: `{"track": {"orgs": ["github.com/entireio"]}}`, want: true, reason: "in tracked org",
		},
		"https origin in the org": {
			origin: "https://github.com/entireio/cli", user: `{"track": {"orgs": ["gh/entireio"]}}`, want: true, reason: "in tracked org",
		},
		"entire origin in the org": {
			origin: "entire://aws-us-east-2.entire.io/gh/entireio/cli", user: `{"track": {"orgs": ["github.com/entireio"]}}`, want: true, reason: "in tracked org",
		},
		"origin outside the org": {
			origin: "git@github.com:other/cli.git", user: `{"track": {"orgs": ["github.com/entireio"]}}`, reason: "not in a tracked org",
		},
		"org prefix does not match a longer owner": {
			origin: "git@github.com:entireio-fork/cli.git", user: `{"track": {"orgs": ["github.com/entireio"]}}`, reason: "not in a tracked org",
		},
		"explicit allow outside the org": {
			origin: "git@github.com:other/cli.git", user: `{"track": {"orgs": ["github.com/entireio"], "repos": ["gh/other/cli"]}}`, want: true, reason: "by name",
		},
		"repos enabled true counts as an explicit allow": {
			origin: "git@github.com:other/cli.git",
			user:   `{"repos": {"gh/other/cli": {"enabled": true}}, "track": {"orgs": ["github.com/entireio"]}}`, want: true, reason: "enabled in this repository",
		},
		"local enabled true outside the org is tracked": {
			origin: "git@github.com:other/cli.git", local: `{"enabled": true}`,
			user: `{"global_enable": true, "track": {"orgs": ["github.com/entireio"]}}`, want: true, reason: "enabled in this repository",
		},
		"deny still excludes a locally enabled repository": {
			origin: "git@github.com:other/cli.git", local: `{"enabled": true}`,
			user: `{"global_enable": true, "track": {"deny": ["gh/other/cli"]}}`, reason: "track.deny",
		},
		"deny still excludes a repos-enabled repository": {
			origin: "git@github.com:other/cli.git",
			user:   `{"repos": {"gh/other/cli": {"enabled": true}}, "track": {"deny": ["gh/other/cli"]}}`, reason: "track.deny",
		},
		"an explicit enable overrides a committed enabled false": {
			origin: "git@github.com:other/cli.git", committed: `{"enabled": false}`, local: `{"enabled": true}`,
			user: `{"track": {"orgs": ["github.com/entireio"]}}`, want: true, reason: "enabled in this repository",
		},
		"local enabled false is not tracked": {
			origin: "git@github.com:entireio/cli.git", local: `{"enabled": false}`,
			user: `{"track": {"orgs": ["github.com/entireio"]}}`, reason: "settings.local.json",
		},
		"local file without enabled is not an explicit enable": {
			origin: "git@github.com:other/cli.git", local: `{"commit_linking": "always"}`,
			user: `{"track": {"orgs": ["github.com/entireio"]}}`, reason: "not in a tracked org",
		},
		"no remote under an org rule": {
			user: `{"track": {"orgs": ["github.com/entireio"]}}`, reason: "not in a tracked org",
		},
		"invalid track block fails closed": {
			origin: "git@github.com:entireio/cli.git", user: `{"track": {"org": ["x"]}}`, reason: "invalid",
		},
		// .entire/settings.json does not know `track`; whatever a repository
		// commits there, the rule comes only from the user file.
		"a committed track block authorizes nothing": {
			origin: "git@github.com:other/cli.git", committed: `{"track": {"orgs": ["github.com/other"]}}`,
			user: `{"track": {"orgs": ["github.com/entireio"]}}`, reason: "not in a tracked org",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := newTrackRepo(t, tc.origin)
			writeUserSettings(t, tc.user)
			if tc.committed != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, EntireSettingsFile), []byte(tc.committed), 0o600))
			}
			if tc.local != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, EntireSettingsLocalFile), []byte(tc.local), 0o600))
			}
			ok, reason := trackable(t, root)
			assert.Equal(t, tc.want, ok, reason)
			assert.Contains(t, reason, tc.reason)
		})
	}
}

// A repository with no remote is denied, or allowed, by its path, from any
// worktree of the clone.
func TestRepoTrackable_PathKeys(t *testing.T) {
	root := newTrackRepo(t, "")
	writeUserSettings(t, `{"track": {"deny": [`+jsonString(root)+`]}}`)
	ok, reason := trackable(t, root)
	assert.False(t, ok)
	assert.Contains(t, reason, "track.deny")

	writeUserSettings(t, `{"track": {"orgs": ["github.com/entireio"], "repos": [`+jsonString(root)+`]}}`)
	ok, _ = trackable(t, root)
	assert.True(t, ok)
}

// Deny, allow, and the org rule round-trip through the user file, which
// keeps its other blocks, and an emptied rule removes the block.
func TestModifyTrackRule(t *testing.T) {
	newTrackRepo(t, "")
	writeUserSettings(t, `{"global_enable": true}`)
	_, err := ModifyTrackRule(t.Context(), func(r *TrackRule) { r.Deny = append(r.Deny, "gh/a/b") })
	require.NoError(t, err)
	assert.JSONEq(t, `{"global_enable": true, "track": {"deny": ["gh/a/b"]}}`, readUserSettings(t))
	rule, err := LoadTrackRule(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"gh/a/b"}, rule.Deny)
	_, err = ModifyTrackRule(t.Context(), func(r *TrackRule) { *r = TrackRule{} })
	require.NoError(t, err)
	assert.JSONEq(t, `{"global_enable": true}`, readUserSettings(t))
}

func TestHookRepoTrackable_OnlyNarrowsGlobalEnable(t *testing.T) {
	root := newTrackRepo(t, "git@github.com:other/cli.git")
	writeUserSettings(t, `{"track": {"deny": ["gh/other/cli"]}}`)
	ClearTrackCache()
	assert.True(t, HookRepoTrackable(t.Context(), root), "global enable off: the rule does not apply")
	writeUserSettings(t, `{"global_enable": true, "track": {"deny": ["gh/other/cli"]}}`)
	ClearTrackCache()
	assert.False(t, HookRepoTrackable(t.Context(), root))
}

// A repository the developer enabled itself is on whatever the org rule says;
// only track.deny excludes it.
func TestHookRepoTrackable_ExplicitEnableOverridesOrgRule(t *testing.T) {
	root := newTrackRepo(t, "git@github.com:other/cli.git")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, EntireSettingsLocalFile), []byte(`{"enabled": true}`), 0o600))
	writeUserSettings(t, `{"global_enable": true, "track": {"orgs": ["github.com/entireio"]}}`)
	ClearTrackCache()
	assert.True(t, HookRepoTrackable(t.Context(), root))
	writeUserSettings(t, `{"global_enable": true, "track": {"orgs": ["github.com/entireio"], "deny": ["gh/other/cli"]}}`)
	ClearTrackCache()
	assert.False(t, HookRepoTrackable(t.Context(), root))
}
