package settings

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// loadedSymlinkedAgentDirs runs the real merge path and reports the effective
// list plus its rejection, if any.
//
// Not t.Parallel-safe, unlike the external_agents tests beside it: the load
// installs the surviving set as agent's process-wide policy, so two of these
// running at once would overwrite each other's. The cleanup restores the strict
// default for everything else in the package.
func loadedSymlinkedAgentDirs(t *testing.T, projectPath, localPath string) ([]string, string, bool) {
	t.Helper()
	t.Cleanup(func() { agent.SetVouchedSymlinkedDirs("", nil) })
	s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
	require.NoError(t, err)
	reason, rejected := s.SymlinkedAgentDirsRejection()
	return s.AllowSymlinkedAgentDirs, reason, rejected
}

// The whole point of the gate. A repository that could ship a symlink at
// .claude AND vouch for it in the committed settings file would have `entire
// enable` write through it on every developer who cloned.
func TestSymlinkedAgentDirsTrust_ProjectSettingIsIgnored(t *testing.T) {
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"allow_symlinked_agent_dirs":[".claude"]}`)

	dirs, reason, rejected := loadedSymlinkedAgentDirs(t, project, local)

	assert.Empty(t, dirs, "a grant from the committed project file must be dropped")
	assert.True(t, rejected, "the rejection must be reportable")
	assert.Contains(t, reason, "settings.local.json", "the reason names where the setting must live")
	assert.Empty(t, agent.VouchedSymlinkedDirs(filepath.Dir(filepath.Dir(project))), "and nothing may be vouched for in the agent package")
}

func TestSymlinkedAgentDirsTrust_UntrackedLocalSettingIsHonored(t *testing.T) {
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"allow_symlinked_agent_dirs":[".claude"]}`)

	dirs, _, rejected := loadedSymlinkedAgentDirs(t, project, local)

	assert.Equal(t, []string{".claude"}, dirs, "an untracked local override is developer-owned")
	assert.False(t, rejected)
	assert.Equal(t, []string{".claude"}, agent.VouchedSymlinkedDirs(filepath.Dir(filepath.Dir(project))),
		"the load must install the policy, or the setting is inert")
}

func TestSymlinkedAgentDirsTrust_StagedLocalFileIsIgnored(t *testing.T) {
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"allow_symlinked_agent_dirs":[".claude"]}`)

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)

	dirs, _, _ := loadedSymlinkedAgentDirs(t, project, local)

	assert.Empty(t, dirs, "a local file tracked in the index must not be trusted")
	assert.Empty(t, agent.VouchedSymlinkedDirs(filepath.Dir(filepath.Dir(project))))
}

// The second, independent boundary: the trust gate says whether the FILE may
// grant, and the name check says whether the PATH is one an agent config lives
// in. A developer's own verified local file still cannot vouch for .entire.
func TestSymlinkedAgentDirsTrust_LocalFileStillCannotVouchForEntireDir(t *testing.T) {
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"allow_symlinked_agent_dirs":[".entire",".git/hooks",".claude"]}`)

	_, reason, rejected := loadedSymlinkedAgentDirs(t, project, local)

	assert.True(t, rejected, "the unusable entries must be reported, not silently dropped")
	assert.Contains(t, reason, ".entire")
	assert.Contains(t, reason, ".git/hooks")
	assert.Equal(t, []string{".claude"}, agent.VouchedSymlinkedDirs(filepath.Dir(filepath.Dir(project))),
		"the legitimate entry survives; only the unspellable ones are refused")
}

// An empty or absent list grants nothing, so it must not produce a warning
// about a privilege nobody asked for.
func TestSymlinkedAgentDirsTrust_AbsentSettingIsSilent(t *testing.T) {
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)

	dirs, reason, rejected := loadedSymlinkedAgentDirs(t, project, local)

	assert.Empty(t, dirs)
	assert.False(t, rejected, "no setting is not a rejected setting")
	assert.Empty(t, reason)
}

// Every load reinstalls the policy, including one that produced nothing, so a
// process whose settings stop granting cannot keep following the old link.
func TestSymlinkedAgentDirsTrust_LoadClearsAPreviousGrant(t *testing.T) {
	t.Cleanup(func() { agent.SetVouchedSymlinkedDirs("", nil) })

	_, project, local := newOPFRepo(t)
	root := filepath.Dir(filepath.Dir(project))

	agent.SetVouchedSymlinkedDirs(root, []string{".claude"})
	require.NotEmpty(t, agent.VouchedSymlinkedDirs(root))

	writeSettingsFile(t, project, `{"enabled":true}`)
	_, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Empty(t, agent.VouchedSymlinkedDirs(root),
		"a load with no grant must clear one installed earlier in the process")
}

// A policy loaded for one worktree must not decide anything in another. The
// enforcement path takes a worktreeRoot precisely so last-load-wins cannot
// follow a link the other tree vouched for, and refusing is the safe direction.
func TestSymlinkedAgentDirsTrust_PolicyIsScopedToItsWorktree(t *testing.T) {
	t.Cleanup(func() { agent.SetVouchedSymlinkedDirs("", nil) })

	_, project, local := newOPFRepo(t)
	root := filepath.Dir(filepath.Dir(project))
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"allow_symlinked_agent_dirs":[".claude"]}`)

	_, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	require.Equal(t, []string{".claude"}, agent.VouchedSymlinkedDirs(root),
		"sanity: the policy is installed for the tree it was loaded for")

	assert.Empty(t, agent.VouchedSymlinkedDirs(t.TempDir()),
		"another worktree must not inherit this one's grant")
}
