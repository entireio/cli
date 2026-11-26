package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The picker starts with every repository selected except those already
// denied, and a deselection is written to deny.
func TestTrackSelect_PreselectedAndDeselectDenies(t *testing.T) {
	t.Parallel()
	candidates := []string{"gh/a/one", "gh/a/two", "/src/three"}
	rule := settings.TrackRule{Deny: []string{"/src/three", "gh/other/kept"}}

	options, selected := trackSelectOptions(candidates, rule)
	require.Len(t, options, 3)
	assert.Equal(t, []string{"gh/a/one", "gh/a/two"}, selected)

	// The user deselects two and re-selects the previously denied one.
	applyTrackSelection(&rule, candidates, []string{"/src/three"})
	assert.ElementsMatch(t, []string{"gh/other/kept", "gh/a/one", "gh/a/two"}, rule.Deny)
}

func TestTrackDenyAndAllow(t *testing.T) {
	t.Parallel()
	rule := settings.TrackRule{Repos: []string{"gh/a/b"}}
	denyTrackRepo(&rule, "gh/a/b")
	assert.Empty(t, rule.Repos)
	assert.Equal(t, []string{"gh/a/b"}, rule.Deny)
	allowTrackRepo(&rule, "gh/a/b")
	assert.Empty(t, rule.Deny)
	assert.Empty(t, rule.Repos, "without an org rule, allow only lifts a deny")
	rule.Orgs = []string{"gh/x"}
	allowTrackRepo(&rule, "gh/a/b")
	assert.Equal(t, []string{"gh/a/b"}, rule.Repos)
}

// deny by path, org add, list --json and reset work without a terminal.
func TestTrackCommands_NonInteractive(t *testing.T) {
	entireDir, _, _, out, err := runGlobalCmd(t, false, `{"global_enable": true}`, "track", "deny", ".")
	require.NoError(t, err, out)
	t.Cleanup(settings.ClearTrackCache)
	rule, err := settings.LoadTrackRule(t.Context())
	require.NoError(t, err)
	require.Len(t, rule.Deny, 1, "a repository without a remote is denied by its path")

	out, err = runRootCmd(t, "track", "list", "--json")
	require.NoError(t, err, out)
	var listed trackListJSON
	require.NoError(t, json.Unmarshal([]byte(out), &listed), out)
	require.NotNil(t, listed.Current)
	assert.False(t, listed.Current.Tracked)
	assert.Contains(t, listed.Current.Reason, "track.deny")

	out, err = runRootCmd(t, "status", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"tracked":false`)
	assert.Contains(t, out, `"track_reason":"denied by track.deny"`)

	out, err = runRootCmd(t, "track", "org", "add", "https://github.com/EntireIO")
	require.NoError(t, err, out)
	out, err = runRootCmd(t, "track", "reset")
	require.NoError(t, err, out)
	assert.JSONEq(t, `{"global_enable": true}`, readUserSettingsFile(t, entireDir))

	_, err = runRootCmd(t, "track", "org", "add", "nonsense")
	require.Error(t, err)
}

// `enable --global --track-org` sets the org rule without a prompt; --yes
// alone keeps allow-all.
func TestEnableGlobal_TrackOrg(t *testing.T) {
	entireDir, _, _, out, err := runGlobalCmd(t, false, "", "enable", "--global", "--yes", "--track-org", "github.com/entireio")
	require.NoError(t, err, out)
	assert.JSONEq(t, `{"global_enable": true, "track": {"orgs": ["gh/entireio"]}}`, readUserSettingsFile(t, entireDir))

	entireDir, _, _, out, err = runGlobalCmd(t, false, "", "enable", "--global", "--yes")
	require.NoError(t, err, out)
	assert.JSONEq(t, `{"global_enable": true}`, readUserSettingsFile(t, entireDir))
}

func TestCurrentRepoOrg(t *testing.T) {
	_, _, repo, out, err := runGlobalCmd(t, false, "", "version")
	require.NoError(t, err, out)
	testutil.RunGit(t, repo, "remote", "add", "origin", "git@github.com:EntireIO/cli.git")
	settings.ClearOriginKeyCache()
	assert.Equal(t, "gh/entireio", currentRepoOrg(t.Context()))
}

// runRootCmd runs the root command in the current directory.
func runRootCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	settings.ClearTrackCache()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}
