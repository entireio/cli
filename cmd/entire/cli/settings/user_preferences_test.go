package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// These tests set ENTIRE_CONFIG_DIR (t.Setenv) and cannot run in parallel.

func loadedSettings(t *testing.T, projectPath, localPath string) *EntireSettings {
	t.Helper()
	s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
	require.NoError(t, err)
	return s
}

// Machine-wide preferences layer above the project file: scalars override,
// review profiles merge by name, the summary provider is set.
func TestUserPreferences_LayerAboveTheProjectFile(t *testing.T) {
	setUserSettingsFile(t, `{"preferences":{
		"telemetry": false,
		"log_level": "DEBUG",
		"review_profiles": {"mine": {"task": "be brief"}},
		"review_default_profile": "mine",
		"review_fix_agent": "codex",
		"investigate": {"agents": ["claude-code", "codex"], "max_turns": 3},
		"summary_generation": {"provider": "claude-code"},
		"summary_timeout_seconds": 45
	}}`)
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"telemetry":true,"review_profiles":{"team":{"task":"team task"}},"review_default_profile":"team"}`)

	s := loadedSettings(t, project, local)
	require.NotNil(t, s.Telemetry)
	assert.False(t, *s.Telemetry)
	assert.Equal(t, "DEBUG", s.LogLevel)
	assert.Len(t, s.ReviewProfiles, 2, "user profiles merge with, not replace, team profiles")
	assert.Equal(t, "be brief", s.ReviewProfiles["mine"].Task)
	assert.Equal(t, "team task", s.ReviewProfiles["team"].Task)
	assert.Equal(t, "mine", s.ReviewDefaultProfile)
	assert.Equal(t, "codex", s.ReviewFixAgent)
	require.NotNil(t, s.Investigate)
	assert.Equal(t, 3, s.Investigate.MaxTurns)
	require.NotNil(t, s.SummaryGeneration)
	assert.Equal(t, "claude-code", s.SummaryGeneration.Provider)
	assert.Equal(t, 45, s.SummaryTimeoutSeconds)
	assert.True(t, s.Enabled, "the user tier cannot touch activation")
	assert.Empty(t, s.UserLayerRejections())
}

// project < preferences < repos[<this repo>] < local.
func TestUserPreferences_PrecedenceAgainstReposAndLocal(t *testing.T) {
	root, project, local := newOPFRepo(t)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	setUserSettingsFile(t, `{
		"preferences": {"review_default_profile": "machine"},
		"repos": {
			"github.com/acme/widgets": {"review_default_profile": "repo"},
			"github.com/acme/other":   {"review_default_profile": "WRONG"}
		}
	}`)
	writeSettingsFile(t, project, `{"enabled":true,"review_default_profile":"team"}`)

	assert.Equal(t, "repo", loadedSettings(t, project, local).ReviewDefaultProfile, "the matching repos entry beats machine-wide preferences")

	writeSettingsFile(t, local, `{"review_default_profile":"worktree"}`)
	assert.Equal(t, "worktree", loadedSettings(t, project, local).ReviewDefaultProfile, "the local file beats the user tier")
}

// The origin key is case-insensitive and covers push URLs, exactly like
// exclude_origins and trusted_origins.
func TestUserPreferences_ReposKeyMatchesAnyOriginURLCaseInsensitively(t *testing.T) {
	root, project, local := newOPFRepo(t)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	testutil.RunGit(t, root, "remote", "set-url", "--push", "origin", "git@github.com:Acme/Widgets-Mirror.git")
	writeSettingsFile(t, project, `{"enabled":true}`)

	setUserSettingsFile(t, `{"repos":{"GitHub.com/Acme/Widgets":{"review_fix_agent":"codex"}}}`)
	assert.Equal(t, "codex", loadedSettings(t, project, local).ReviewFixAgent)

	setUserSettingsFile(t, `{"repos":{"github.com/acme/widgets-mirror":{"review_fix_agent":"gemini"}}}`)
	assert.Equal(t, "gemini", loadedSettings(t, project, local).ReviewFixAgent, "a push URL keys the repo too")
}

// A repository with no origin is keyed by its worktree path, the way
// trusted_paths keys it.
func TestUserPreferences_ReposKeyByPathWhenThereIsNoOrigin(t *testing.T) {
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	setUserSettingsFile(t, `{"repos":{"`+root+`":{"review_fix_agent":"codex"},"/definitely/elsewhere":{"review_fix_agent":"WRONG"}}}`)

	assert.Equal(t, "codex", loadedSettings(t, project, local).ReviewFixAgent)
}

// The allowlist is the boundary: a key that would activate tracking, change
// redaction, or move checkpoints is unknown here and rejects the block — and
// only the block. Settings still load, and the rejection is reported.
func TestUserPreferences_UnknownKeyDropsOnlyThatBlock(t *testing.T) {
	root, project, local := newOPFRepo(t)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	writeSettingsFile(t, project, `{"enabled":true,"review_default_profile":"team"}`)
	setUserSettingsFile(t, `{
		"preferences": {"enabled": false, "review_default_profile": "machine"},
		"repos": {
			"github.com/acme/widgets": {"strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "evil/x"}}},
			"github.com/acme/widgets ": {"review_fix_agent": "codex"}
		}
	}`)

	s := loadedSettings(t, project, local)
	assert.True(t, s.Enabled, "an activation key in preferences must not apply")
	assert.Equal(t, "team", s.ReviewDefaultProfile, "the whole preferences block is dropped, not just the bad key")
	assert.Nil(t, s.StrategyOptions, "a checkpoint_remote in a repos entry must not apply")
	assert.Equal(t, "codex", s.ReviewFixAgent, "a valid sibling repos entry still applies")
	rejections := s.UserLayerRejections()
	require.Len(t, rejections, 2)
	assert.Contains(t, rejections[0], "preferences")
	assert.Contains(t, rejections[1], `repos["github.com/acme/widgets"]`)
}

// A user file with no repos block resolves no git remotes at all; one with an
// unrelated repos entry applies nothing.
func TestUserPreferences_NonMatchingReposApplyNothing(t *testing.T) {
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"review_fix_agent":"team"}`)
	setUserSettingsFile(t, `{"repos":{"github.com/acme/other":{"review_fix_agent":"WRONG"}}}`)

	assert.Equal(t, "team", loadedSettings(t, project, local).ReviewFixAgent)
}
