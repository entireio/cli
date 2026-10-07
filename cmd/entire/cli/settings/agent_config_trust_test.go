package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const configWithHook = `{"settings":{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/bin/true"}]}]}}}`

// A reviewer config names commands Entire runs, so one from the committed
// project file is dropped, and the rejection carries no value (it can hold
// secrets).
func TestAgentConfigTrust_ProjectConfigIsDropped(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"review_profiles":{"general":{"agents":{"claude-code":{"config":`+configWithHook+`}}}}}`)

	s := loadedForPromptTrust(t, project, "", local)
	worker, ok := s.ReviewProfiles["general"].Agents["claude-code"]
	require.True(t, ok, "a worker whose only field was a dropped config must stay present")
	assert.Nil(t, worker.Config)
	rejections := s.AgentPromptRejections()
	require.Len(t, rejections, 1)
	assert.Equal(t, "review_profiles.general.agents.claude-code.config", rejections[0].Field)
	assert.Empty(t, rejections[0].Value)
	assert.False(t, AgentConfigRejectionUnverified(rejections[0]))
}

func TestAgentConfigTrust_ClonePreferencesConfigIsHonored(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	prefs := writePreferences(t, `{"review_profiles":{"general":{"agents":{"claude-code":{"config":`+configWithHook+`}}}}}`)

	s := loadedForPromptTrust(t, project, prefs, local)
	cfg := s.ReviewProfiles["general"].Agents["claude-code"].Config
	require.NotNil(t, cfg)
	assert.Contains(t, string(cfg.Settings), "/usr/bin/true")
	assert.Empty(t, s.AgentPromptRejections())
}

func TestAgentConfigTrust_UntrackedLocalConfigIsHonored(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"review_profiles":{"general":{"agents":{"pi":{"config":{"extensions":["/opt/ext.ts"]}}}}}}`)

	s := loadedForPromptTrust(t, project, "", local)
	cfg := s.ReviewProfiles["general"].Agents["pi"].Config
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"/opt/ext.ts"}, cfg.Extensions)
}

// The judge runs from a temp dir and the legacy map predates profiles; a
// config in either is reported, never silently used.
func TestAgentConfigTrust_JudgeAndLegacyConfigAreRejected(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	prefs := writePreferences(t, `{"review_profiles":{"general":{"agents":{"codex":{"model":"x"}},`+
		`"judge":{"agent":"claude-code","config":{}}}},"review":{"pi":{"config":{}}}}`)

	s := loadedForPromptTrust(t, project, prefs, local)
	require.NotNil(t, s.ReviewProfiles["general"].Judge)
	assert.Nil(t, s.ReviewProfiles["general"].Judge.Config)
	assert.Nil(t, s.Review["pi"].Config)
	fields := map[string]bool{}
	for _, rej := range s.AgentPromptRejections() {
		fields[rej.Field] = true
	}
	assert.True(t, fields["review_profiles.general.judge.config"])
	assert.True(t, fields["review.pi.config"])
}
