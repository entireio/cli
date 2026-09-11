//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func enableRemoteSettings(t *testing.T, env *TestEnv, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".entire", name))
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

func assertEnableRemote(t *testing.T, env *TestEnv, remote, source string) {
	t.Helper()
	status := statusSyncJSONOutput(t, env)
	require.Equal(t, remote, status.CheckpointSyncRemote)
	require.Equal(t, source, status.CheckpointSyncRemoteSource)
	require.Empty(t, status.CheckpointSyncError)
}

func TestEnableCheckpointPushRemote_Selection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fresh bool
		agent bool
	}{
		{name: "fresh", fresh: true},
		{name: "reenable"},
		{name: "explicit_agent", fresh: true, agent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// InitRepo configures local identity, disables signing, and isolates hooks.
			env := freshRepoEnv(t)
			env.SetupBareRemote()
			env.SetupNamedBareRemote(forkRemote)
			setBranchTrackingRemote(t, env, "origin")
			if !tc.fresh {
				env.RunCLI("enable", "--yes", "--telemetry=false")
			}
			args := []string{"enable", "--checkpoint-push-remote", forkRemote, "--telemetry=false"}
			if tc.agent {
				args = append(args, "--agent", agentClaudeCode)
			} else {
				args = append(args, "--yes")
			}
			out := env.RunCLI(args...)
			require.Contains(t, out, ".entire/settings.local.json")
			require.Contains(t, out, "Checkpoints will be uploaded when you push to fork.")
			assertEnableRemote(t, env, forkRemote, "config")
			local := enableRemoteSettings(t, env, "settings.local.json")
			options, ok := local["strategy_options"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, forkRemote, options["checkpoint_push_remote"])
		})
	}
}

func TestEnableCheckpointPushRemote_YesPreservesDestination(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		name := "automatic"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			env.SetupBareRemote()
			env.SetupNamedBareRemote(forkRemote)
			setBranchTrackingRemote(t, env, "origin")
			if explicit {
				env.RunCLI("enable", "--yes", "--checkpoint-push-remote", forkRemote)
			}
			before := statusSyncJSONOutput(t, env)
			env.RunCLI("enable", "--yes")
			after := statusSyncJSONOutput(t, env)
			require.Equal(t, before.CheckpointSyncRemote, after.CheckpointSyncRemote)
			require.Equal(t, before.CheckpointSyncRemoteSource, after.CheckpointSyncRemoteSource)
			if explicit {
				assertEnableRemote(t, env, forkRemote, "config")
			} else {
				assertEnableRemote(t, env, "origin", before.CheckpointSyncRemoteSource)
				require.NotEqual(t, "config", after.CheckpointSyncRemoteSource)
			}
		})
	}
}

func TestEnableCheckpointPushRemote_InvalidLeavesFreshRepoUntouched(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{"missing", "", "https://example.invalid/repo.git"} {
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			env := freshRepoEnv(t)
			env.SetupBareRemote()
			out, err := env.RunCLIWithError("enable", "--yes", "--agent", agentClaudeCode, "--checkpoint-push-remote", remote)
			require.Error(t, err, out)
			require.NoDirExists(t, filepath.Join(env.RepoDir, ".entire"))
			require.NoDirExists(t, filepath.Join(env.RepoDir, ".claude"))
			for _, hook := range []string{"pre-push", "post-commit", "prepare-commit-msg"} {
				require.NoFileExists(t, filepath.Join(env.RepoDir, ".git", "hooks", hook))
			}
		})
	}
}

func TestEnableCheckpointPushRemote_ProjectPreservesRawSettings(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	env.SetupNamedBareRemote(forkRemote)
	env.PatchSettings(map[string]any{
		"log_level":        "debug",
		"strategy_options": map[string]any{"future_option": "project", "checkpoint_push_remote": "origin"},
	})
	env.WriteFile(".entire/settings.local.json", `{"enabled":true,"external_agents":true,"log_level":"warn","strategy_options":{"future_option":"local","push_sessions":false}}`)
	projectBefore := enableRemoteSettings(t, env, "settings.json")
	localBefore := enableRemoteSettings(t, env, "settings.local.json")
	env.RunCLI("enable", "--yes", "--project", "--checkpoint-push-remote", forkRemote)
	projectAfter := enableRemoteSettings(t, env, "settings.json")
	localAfter := enableRemoteSettings(t, env, "settings.local.json")
	require.Equal(t, projectBefore, projectAfter)
	options, ok := localBefore["strategy_options"].(map[string]any)
	require.True(t, ok)
	options["checkpoint_push_remote"] = forkRemote
	require.Equal(t, localBefore, localAfter)
	assertEnableRemote(t, env, forkRemote, "config")
}

func TestEnableCheckpointPushRemote_QueuesUntilSelectedPush(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		origin := env.SetupBareRemote()
		fork := env.SetupNamedBareRemote(forkRemote)
		setBranchTrackingRemote(t, env, "origin")
		checkpointID := createCheckpointedCommit(t, env, "Add selected module", "selected.go", "package selected", "Add selected module")
		queuedBefore := queuedCheckpointRefCount(t, env)
		env.RunCLI("enable", "--yes", "--checkpoint-push-remote", forkRemote)
		assertEnableRemote(t, env, forkRemote, "config")
		require.Equal(t, checkpointID, env.LatestCheckpointID())
		require.True(t, env.CheckpointsPresentLocally())
		require.False(t, env.CheckpointsPresentOnRemote(origin))
		require.False(t, env.CheckpointsPresentOnRemote(fork))
		require.Equal(t, queuedBefore, queuedCheckpointRefCount(t, env))
		assertSingleRemoteRouting(t, env, checkpointID,
			remoteTarget{name: "origin", bareDir: origin},
			remoteTarget{name: forkRemote, bareDir: fork}, false)

		// Git invokes the installed pre-push hook with its own argv and stdin.
		nextID := createCheckpointedCommit(t, env, "Add next module", "next.go", "package next", "Add next module")
		env.GitPushWithHooks("origin", "HEAD")
		require.False(t, env.CheckpointExistsOnRemote(origin, nextID))
		require.False(t, env.CheckpointExistsOnRemote(fork, nextID))
		env.GitPushWithHooks(forkRemote, "HEAD")
		require.True(t, env.CheckpointExistsOnRemote(fork, nextID))
		require.False(t, env.CheckpointExistsOnRemote(origin, nextID))
	})
}

func TestEnableCheckpointPushRemote_DisabledPushStaysDisabled(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		origin := env.SetupBareRemote()
		fork := env.SetupNamedBareRemote(forkRemote)
		setBranchTrackingRemote(t, env, "origin")
		env.PatchSettings(map[string]any{"strategy_options": map[string]any{"push_sessions": false}})
		checkpointID := createCheckpointedCommit(t, env, "Add disabled module", "disabled.go", "package disabled", "Add disabled module")
		queuedBefore := queuedCheckpointRefCount(t, env)
		out := env.RunCLI("enable", "--yes", "--checkpoint-push-remote", forkRemote)
		require.Contains(t, out, "Checkpoint pushing remains disabled.")
		require.NotContains(t, out, "Checkpoints will be uploaded")
		assertEnableRemote(t, env, forkRemote, "config")
		env.RunPrePush(forkRemote)
		require.False(t, env.CheckpointsPresentOnRemote(origin))
		require.False(t, env.CheckpointsPresentOnRemote(fork))
		require.Equal(t, checkpointID, env.LatestCheckpointID())
		require.Equal(t, queuedBefore, queuedCheckpointRefCount(t, env))
	})
}
