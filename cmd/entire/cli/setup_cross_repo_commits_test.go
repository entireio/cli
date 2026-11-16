package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func runConfigureCrossRepoCommits(t *testing.T, value string) (string, error) {
	t.Helper()
	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--cross-repo-commits", value})
	err := cmd.Execute()
	return stdout.String(), err
}

func crossRepoCommitsOn(t *testing.T) bool {
	t.Helper()
	commonDir, err := session.GetGitCommonDir(context.Background())
	require.NoError(t, err)
	return settings.LoadCrossRepoCommitsAt(commonDir)
}

// A person at an interactive terminal, outside an agent session, may turn it
// on; off needs neither.
func TestConfigureCmd_CrossRepoCommits(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	clearAgentCallerEnv(t)
	t.Setenv(interactive.EnvTestTTY, "1")

	out, err := runConfigureCrossRepoCommits(t, "on")
	require.NoError(t, err)
	assert.True(t, crossRepoCommitsOn(t))
	assert.Contains(t, out, "Cross-repo commits: on")

	t.Setenv(interactive.EnvTestTTY, "0")
	_, err = runConfigureCrossRepoCommits(t, "off")
	require.NoError(t, err, "off needs no terminal")
	assert.False(t, crossRepoCommitsOn(t))
}

// Agent detection reads the environment, which an agent can strip, so without
// a terminal on is refused even when no agent is detected.
func TestConfigureCmd_CrossRepoCommitsOnNeedsTerminal(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	clearAgentCallerEnv(t)
	t.Setenv(interactive.EnvTestTTY, "0")

	_, err := runConfigureCrossRepoCommits(t, "on")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a person at an interactive terminal")
	assert.NotContains(t, err.Error(), "entire ", "the refusal must not hand the agent a command")
	assert.False(t, crossRepoCommitsOn(t))
}

// An agent must not be able to opt its own sessions in, even with a terminal,
// but it may turn the opt-in off.
func TestConfigureCmd_CrossRepoCommitsOnRefusesAgentSession(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	clearAgentCallerEnv(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv(interactive.EnvTestTTY, "1")

	_, err := runConfigureCrossRepoCommits(t, "on")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to turn cross-repo commits on from an agent session")
	assert.NotContains(t, err.Error(), "entire ", "the refusal must not hand the agent a command")
	assert.False(t, crossRepoCommitsOn(t))

	t.Setenv(interactive.EnvTestTTY, "0")
	_, err = runConfigureCrossRepoCommits(t, "off")
	require.NoError(t, err, "off works in an agent session without a terminal")
}

func TestConfigureCmd_CrossRepoCommitsInvalid(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	clearAgentCallerEnv(t)

	_, err := runConfigureCrossRepoCommits(t, "yes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"on"`)
	assert.False(t, crossRepoCommitsOn(t))
}
