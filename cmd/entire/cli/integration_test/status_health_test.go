//go:build integration

package integration

import (
	"encoding/json"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusIssue2264ReportsBlockedSyncWithoutHooks(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	env.SetupEmptyNamedBareRemote("origin")
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	for _, hook := range strategy.ManagedGitHookNames() {
		require.NoError(t, os.Remove(filepath.Join(env.RepoDir, ".git", "hooks", hook)))
	}

	text := env.RunCLI("status")
	require.NotContains(t, text, "Checkpoints sync to:")
	require.Contains(t, text, "Checkpoint sync blocked")
	require.Contains(t, text, "not installed")

	var got struct {
		CheckpointSyncState  string `json:"checkpoint_sync_state"`
		CheckpointSyncRemote string `json:"checkpoint_sync_remote"`
		GitHooks             struct {
			Mode  string `json:"mode"`
			State string `json:"state"`
		} `json:"git_hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(env.RunCLI("status", "--json")), &got))
	require.Equal(t, "blocked", got.CheckpointSyncState)
	require.Equal(t, "origin", got.CheckpointSyncRemote)
	require.Equal(t, "native", got.GitHooks.Mode)
	require.Equal(t, "absent", got.GitHooks.State)
}

func TestStatusHookHealthReadySubprocessContract(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	env.SetupEmptyNamedBareRemote("origin")
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")

	text := env.RunCLI("status")
	require.True(t, strings.Contains(text, "Checkpoints sync to:") || strings.Contains(text, "Checkpoint sync ready"), text)
	var got struct {
		CheckpointSyncState string `json:"checkpoint_sync_state"`
		GitHooks            struct {
			State string `json:"state"`
		} `json:"git_hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(env.RunCLI("status", "--json")), &got))
	require.Equal(t, "ready", got.CheckpointSyncState)
	require.Equal(t, "current", got.GitHooks.State)
}
