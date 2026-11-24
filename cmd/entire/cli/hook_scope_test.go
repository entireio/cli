package cli

import (
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
