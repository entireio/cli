package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
)

func TestRememberAgentHome_PreservesInvalidRegistry(t *testing.T) {
	for _, data := range []string{"not json", `{"version":2,"homes":{"Claude Code":["/old-home"]}}`} {
		t.Run(data, func(t *testing.T) {
			isolateAgentHomesConfig(t)
			root, err := configRootForTest(t)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			path := filepath.Join(root.Name(), agentHomesFileName)
			require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			require.Error(t, RememberAgentHome(types.AgentType("Claude Code"), t.TempDir()))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, string(after))
		})
	}
}

func TestRememberAgentHome_RefreshesUseAndPrunesMissingHomes(t *testing.T) {
	isolateAgentHomesConfig(t)
	agentType := types.AgentType("Claude Code")
	frequent, gone := t.TempDir(), t.TempDir()
	frequent = canonicalAgentHomeForTest(t, frequent)
	gone = canonicalAgentHomeForTest(t, gone)
	require.NoError(t, RememberAgentHome(agentType, frequent))
	require.NoError(t, RememberAgentHome(agentType, gone))
	require.NoError(t, os.Remove(gone))
	for range maxHomesPerAgent {
		require.NoError(t, RememberAgentHome(agentType, frequent))
		require.NoError(t, RememberAgentHome(agentType, t.TempDir()))
	}
	homes := KnownAgentHomes(agentType)
	require.Len(t, homes, maxHomesPerAgent)
	require.Contains(t, homes, frequent)
	root, err := configRootForTest(t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	file, err := readAgentHomesFile(root)
	require.NoError(t, err)
	require.NotContains(t, file.Homes[agentType], gone)
}
func TestRememberAgentHome_PreservesInaccessibleHome(t *testing.T) {
	isolateAgentHomesConfig(t)
	parent := t.TempDir()
	historical := filepath.Join(parent, "historical")
	require.NoError(t, os.Mkdir(historical, 0700))
	historical = canonicalAgentHomeForTest(t, historical)
	kind := types.AgentType("Claude Code")
	require.NoError(t, RememberAgentHome(kind, historical))
	require.NoError(t, os.Chmod(parent, 0000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0700)) })
	_, statErr := os.Stat(historical)
	if statErr == nil {
		t.Skip("filesystem does not enforce directory permissions")
	}
	require.ErrorIs(t, statErr, os.ErrPermission)
	require.NoError(t, RememberAgentHome(kind, t.TempDir()))
	require.NoError(t, os.Chmod(parent, 0700))
	require.Contains(t, KnownAgentHomes(kind), historical, "temporary stat failure must not revoke historical-home trust")
}
