//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A repository enabled only through the user settings file has no .entire of
// its own. The hook path must still read the user tier and record the session;
// otherwise every hook is a silent no-op for exactly the setup the tier exists
// for.
func TestUserTier_EnablesHooksWithoutRepoSettings(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "init")
	env.GitAdd("README.md")
	env.GitCommit("init")
	require.NoDirExists(t, filepath.Join(env.RepoDir, ".entire"),
		"the premise: nothing in the worktree enables Entire")

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	prompt := func(sessionID, configDir string) {
		t.Helper()
		input, err := json.Marshal(map[string]string{"session_id": sessionID, "transcript_path": ""})
		require.NoError(t, err)
		require.NoError(t, runner.runHookInRepoDirWithExtraEnv("user-prompt-submit", input,
			[]string{"ENTIRE_CONFIG_DIR=" + configDir}))
	}

	prompt("user-tier-off", t.TempDir())
	state, err := env.GetSessionState("user-tier-off")
	require.NoError(t, err)
	require.Nil(t, state, "sanity: with no user settings the hook does nothing")

	configDir := t.TempDir()
	userSettings, err := json.Marshal(map[string]any{
		"repos": map[string]any{env.RepoDir: map[string]any{"enabled": true}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "settings.json"), userSettings, 0o600))

	prompt("user-tier-on", configDir)
	state, err = env.GetSessionState("user-tier-on")
	require.NoError(t, err)
	require.NotNil(t, state, "a repository enabled in the user settings file records the session")
}
