package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// Under an org rule the picker shows what is actually tracked: a repository
// outside the orgs starts deselected, selecting it allows it by name, and
// deselecting then reselecting any candidate round-trips to the same rule.
func TestTrackSelect_OrgRulePreselectsTrackedAndRoundTrips(t *testing.T) {
	t.Parallel()
	inOrg, outside, named := "gh/acme/app", "gh/other/lib", "gh/other/named"
	candidates := []string{inOrg, outside, named}
	rule := settings.TrackRule{Orgs: []string{"gh/acme"}, Repos: []string{named}}

	_, selected := trackSelectOptions(candidates, rule)
	assert.Equal(t, []string{inOrg, named}, selected, "the repository outside the org is not tracked, so not pre-selected")

	applyTrackSelection(&rule, candidates, candidates)
	assert.Empty(t, rule.Deny)
	assert.ElementsMatch(t, []string{named, outside}, rule.Repos, "selecting the outside repository allows it by name")
	_, selected = trackSelectOptions(candidates, rule)
	assert.Equal(t, candidates, selected)

	before := settings.TrackRule{Orgs: slices.Clone(rule.Orgs), Repos: slices.Clone(rule.Repos)}
	applyTrackSelection(&rule, candidates, nil)
	_, selected = trackSelectOptions(candidates, rule)
	assert.Empty(t, selected, "every deselected candidate is untracked")
	applyTrackSelection(&rule, candidates, candidates)
	assert.Empty(t, rule.Deny)
	assert.ElementsMatch(t, before.Repos, rule.Repos)
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
	assert.Contains(t, out, `"enabled":false`, "hooks treat an untracked repository as not enabled")

	out, err = runRootCmd(t, "status")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Not tracked (denied by track.deny")
	assert.NotContains(t, out, "Enabled")

	out, err = runRootCmd(t, "track", "org", "add", "https://github.com/EntireIO")
	require.NoError(t, err, out)
	out, err = runRootCmd(t, "track", "reset")
	require.NoError(t, err, out)
	assert.JSONEq(t, `{"global_enable": true}`, readUserSettingsFile(t, entireDir))

	_, err = runRootCmd(t, "track", "org", "add", "nonsense")
	require.Error(t, err)
}

// A repository the developer enabled itself is tracked outside the org rule,
// and status says Enabled; track.deny still excludes it.
func TestStatus_ExplicitEnableOverridesOrgRule(t *testing.T) {
	_, _, repo, out, err := runGlobalCmd(t, false, `{"global_enable": true, "track": {"orgs": ["github.com/entireio"]}}`, "version")
	require.NoError(t, err, out)
	t.Cleanup(settings.ClearTrackCache)

	out, err = runRootCmd(t, "status", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"enabled":false`, "on only through global enable and outside the org")
	assert.Contains(t, out, `"tracked":false`)

	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, settings.EntireSettingsLocalFile), []byte(`{"enabled": true}`), 0o600))
	out, err = runRootCmd(t, "status", "--json")
	require.NoError(t, err, out)
	var got statusJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.True(t, got.Enabled)
	assert.Empty(t, got.EnabledBy)
	require.NotNil(t, got.Tracked)
	assert.True(t, *got.Tracked)

	out, err = runRootCmd(t, "status")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Enabled")
	assert.NotContains(t, out, "Not tracked")
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

// An invalid --track-org is refused before global_enable is written, so it
// never leaves global enable on without its rule or user-level hooks.
func TestEnableGlobal_InvalidTrackOrgChangesNothing(t *testing.T) {
	entireDir, claudeDir, _, out, err := runGlobalCmd(t, true, `{"preferences": {"telemetry": false}}`, "enable", "--global", "--yes", "--track-org", "/")
	require.Error(t, err, out)
	assert.JSONEq(t, `{"preferences": {"telemetry": false}}`, readUserSettingsFile(t, entireDir))
	if _, statErr := os.Stat(filepath.Join(claudeDir, "settings.json")); !os.IsNotExist(statErr) {
		t.Errorf("hooks installed although nothing was enabled (stat err = %v)", statErr)
	}
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

// A repository the tracking rule excludes runs no git hooks either, as its
// agent hooks record nothing: sessions recorded before the rule changed are
// neither condensed nor pushed there.
func TestHooksGit_TrackingRuleExcludesRepository(t *testing.T) {
	for name, tc := range map[string]struct {
		rule         string
		wantDisabled bool
	}{
		"outside the org rule": {`, "track": {"orgs": ["gh/acme"]}`, true},
		"every repository":     {``, false},
	} {
		t.Run(name, func(t *testing.T) {
			gitHooksDisabled = false
			t.Cleanup(func() { gitHooksDisabled = false })
			settings.ClearTrackCache()
			t.Cleanup(settings.ClearTrackCache)
			_, _, _, out, err := runGlobalCmd(t, false, `{"global_enable": true`+tc.rule+`}`, "hooks", "git", "post-commit")
			require.NoError(t, err, out)
			assert.Equal(t, tc.wantDisabled, gitHooksDisabled)
		})
	}
}
