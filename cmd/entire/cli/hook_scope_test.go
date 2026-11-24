package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// scopedHookCmd is a hook verb under `entire hooks` with --scope parsed.
func scopedHookCmd(t *testing.T, scope string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "hooks"}
	root.PersistentFlags().String(hooksScopeFlag, "", "")
	verb := &cobra.Command{Use: "stop", RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(verb)
	args := []string{"stop"}
	if scope != "" {
		args = append(args, "--"+hooksScopeFlag+"="+scope)
	}
	root.SetArgs(args)
	root.SetContext(context.Background())
	require.NoError(t, root.Execute())
	return verb
}

// A Codex hook from the user-level hooks.json steps aside exactly when the
// checkout's own Entire hooks are installed and approved; a project hook
// (no --scope) never does.
func TestUserScopeStepsAside_CodexFollowsTheTrustedProjectInstall(t *testing.T) {
	t.Cleanup(osroot.ResetShared)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repo := t.TempDir()
	testutil.InitRepo(t, repo)
	t.Chdir(repo)

	userScope := codex.UserScopePrefix + "1"
	assert.False(t, userScopeStepsAside(scopedHookCmd(t, userScope), agent.AgentNameCodex), "no project install")

	_, err := (&codex.CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	assert.False(t, userScopeStepsAside(scopedHookCmd(t, userScope), agent.AgentNameCodex), "project hooks not approved")

	discovered := codex.ResolveHookDiscovery(context.Background()).DiscoveredHooks.Path()
	var trust strings.Builder
	for _, spec := range codex.HookEventSpecs() {
		fmt.Fprintf(&trust, "[hooks.state.%q]\ntrusted_hash = \"x\"\n", discovered+":"+spec.Label+":0:0")
	}
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(trust.String()), 0o600))

	assert.True(t, userScopeStepsAside(scopedHookCmd(t, userScope), agent.AgentNameCodex), "approved project hooks take over")
	assert.False(t, userScopeStepsAside(scopedHookCmd(t, ""), agent.AgentNameCodex), "a project hook never steps aside")
}

// With global enable on, every agent's hooks run in every directory the
// agent is started in, including ones outside any git repository. There they
// do nothing and fail nothing.
func TestAgentHooks_OutsideARepositoryDoNothing(t *testing.T) {
	entireDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", entireDir)
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(`{"global_enable": true}`), 0o600))
	isolateAgentUserHomes(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	dir := t.TempDir()
	t.Chdir(dir)

	for _, args := range [][]string{
		{"hooks", "claude-code", "session-start"},
		{"hooks", "claude-code", "user-prompt-submit"},
		{"hooks", "codex", "--scope=" + codex.UserScopePrefix + "1", "session-start"},
		{"hooks", "pi", "session_start"},
		{"hooks", "opencode", "session-start"},
	} {
		root := NewRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetIn(strings.NewReader(`{"session_id":"s1","prompt":"hi"}`))
		root.SetArgs(args)
		require.NoError(t, root.Execute(), "%v: %s", args, out.String())
		assert.Empty(t, out.String(), "%v", args)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing written outside a repository")
}

// The lazy setup on an agent's first prompt never installs git hooks where
// the repository is off despite global enable: a committed `enabled: false`,
// or a `repos` entry from `entire disable`.
func TestTurnStart_GlobalEnableDoesNotSetUpADisabledRepository(t *testing.T) {
	for name, disable := range map[string]func(t *testing.T, repo string){
		"control: not disabled": nil,
		"committed enabled false": func(t *testing.T, repo string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".entire"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".entire", "settings.json"), []byte(`{"enabled": false}`), 0o600))
		},
		"repos entry disable": func(t *testing.T, repo string) {
			t.Helper()
			_, err := settings.SetRepoEnabled(context.Background(), repo, false)
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			entireDir := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", entireDir)
			require.NoError(t, os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(`{"global_enable": true}`), 0o600))
			isolateAgentUserHomes(t)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
			t.Cleanup(settings.ClearOriginKeyCache)
			repo := t.TempDir()
			testutil.InitRepo(t, repo)
			t.Chdir(repo)
			if disable != nil {
				disable(t, repo)
			}
			settings.ClearOriginKeyCache()

			root := NewRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetIn(strings.NewReader(`{"session_id":"s1","prompt":"hi","transcript_path":""}`))
			root.SetArgs([]string{"hooks", "claude-code", "user-prompt-submit"})
			require.NoError(t, root.Execute(), out.String())
			if disable == nil {
				assert.True(t, strategy.IsGitHookInstalled(context.Background()), "the control repository is set up")
				return
			}
			assert.False(t, strategy.IsGitHookInstalled(context.Background()), "git hooks installed in a disabled repository")
			assert.NoDirExists(t, filepath.Join(repo, ".entire", "metadata"))
		})
	}
}
