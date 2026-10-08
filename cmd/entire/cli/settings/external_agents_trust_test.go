package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// loadedExternalAgents runs the real merge path and reports the effective
// setting plus its rejection, if any.
func loadedExternalAgents(t *testing.T, projectPath, localPath string) (bool, string, bool) {
	t.Helper()
	s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
	require.NoError(t, err)
	reason, rejected := s.ExternalAgentsRejection()
	return s.ExternalAgents, reason, rejected
}

// external_agents turns on $PATH scanning and execution of entire-agent-*
// binaries. Delivered through the version-controlled project file it is an
// ordinary-looking JSON diff that grants exec, so it must not take effect.
func TestExternalAgentsTrust_ProjectSettingIsIgnored(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"external_agents":true}`)

	enabled, reason, rejected := loadedExternalAgents(t, project, local)

	assert.False(t, enabled, "external_agents from the committed project file must be dropped")
	assert.True(t, rejected, "the rejection must be reportable to the consumer")
	assert.Contains(t, reason, "settings.local.json", "the reason names where the setting must live")
}

func TestExternalAgentsTrust_UntrackedLocalSettingIsHonored(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"external_agents":true}`)

	enabled, _, rejected := loadedExternalAgents(t, project, local)

	assert.True(t, enabled, "an untracked local override is developer-owned and must be honored")
	assert.False(t, rejected, "a trusted setting must not be reported as rejected")
}

func TestExternalAgentsTrust_StagedLocalFileIsIgnored(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"external_agents":true}`)

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.False(t, s.ExternalAgents, "a local file tracked in the index must not be trusted")
	// The whole layer is already dropped here, so the user's signal is the
	// layer rejection: reporting the grant separately would say the same
	// thing twice about one file.
	assert.NotEmpty(t, s.LocalLayerRejection(), "the tracked layer must still be reported")
	_, rejected := s.ExternalAgentsRejection()
	assert.False(t, rejected, "the layer rejection already names the file and the fix")
}

func TestExternalAgentsTrust_CommittedThenUnstagedIsIgnored(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"external_agents":true}`)

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")
	testutil.RunGit(t, root, "rm", "--cached", EntireSettingsLocalFile)

	enabled, _, _ := loadedExternalAgents(t, project, local)

	assert.False(t, enabled, "content still reachable from HEAD must not be trusted")
}

// With no repository there is nothing to have cloned the file from, so it is
// definitively this developer's own.
func TestExternalAgentsTrust_OutsideGitRepoHonorsLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	project := filepath.Join(dir, EntireSettingsFile)
	local := filepath.Join(dir, EntireSettingsLocalFile)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"external_agents":true}`)

	enabled, _, rejected := loadedExternalAgents(t, project, local)

	assert.True(t, enabled, "with no repository the local file cannot have arrived by cloning")
	assert.False(t, rejected, "no rejection when there is nothing to distrust")
}

// Same divergence as the OPF command: an unreadable repository keeps the
// layer but fails closed on the exec-granting setting.
func TestExternalAgentsTrust_UnverifiableRepoKeepsLayerDropsSetting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755)) // present but not a repo
	project := filepath.Join(dir, EntireSettingsFile)
	local := filepath.Join(dir, EntireSettingsLocalFile)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"external_agents":true,"commit_linking":"always"}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.False(t, s.ExternalAgents, "an unverifiable repo must not grant PATH execution")
	assert.Equal(t, "always", s.CommitLinking, "unrelated local settings must survive")
	assert.Empty(t, s.LocalLayerRejection(), "the layer itself was kept")
}

// A local file that says nothing about external_agents does not vouch for a
// project file that does.
func TestExternalAgentsTrust_LocalFileWithoutSettingDoesNotVouch(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"external_agents":true}`)
	writeSettingsFile(t, local, `{"commit_linking":"always"}`)

	enabled, _, rejected := loadedExternalAgents(t, project, local)

	assert.False(t, enabled, "presence of an unrelated local file is not consent")
	assert.True(t, rejected, "the rejection must be reportable")
}

// Off is the default and grants nothing, so there is nothing to gate: a
// project file that leaves it off must not produce a rejection notice.
func TestExternalAgentsTrust_DisabledSettingIsNotRejected(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"external_agents":false}`)

	enabled, _, rejected := loadedExternalAgents(t, project, local)

	assert.False(t, enabled)
	assert.False(t, rejected, "a setting that grants nothing needs no warning")
}
