package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// setUserSettings points the user config and cache at temp dirs and writes
// content as the user settings file.
func setUserSettings(t *testing.T, content string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(content), 0o600))
}

// noticeCmd is a session-start hook command whose stdin carries sessionID.
func noticeCmd(sessionID string) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(`{"session_id":"` + sessionID + `","transcript_path":""}`))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	return cmd, &stderr
}

// reportCapturingStdout runs reportUnreadableUserSettings and returns what it
// wrote to stdout (the hook response) and stderr.
func reportCapturingStdout(t *testing.T, agentName types.AgentName, sessionID string, setUp bool) (string, string) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	oldOut := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldOut })
	cmd, stderr := noticeCmd(sessionID)
	reportUnreadableUserSettings(cmd, agentName, setUp)
	os.Stdout = oldOut
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out), stderr.String()
}

// Not parallel: sets the user config and cache env and swaps os.Stdout.
func TestReportUnreadableUserSettings_BannerOncePerSessionWhenNotSetUp(t *testing.T) {
	setUserSettings(t, "{not json")

	out, stderr := reportCapturingStdout(t, agent.AgentNameClaudeCode, "notice-session-1", false)
	assert.Contains(t, out, "could not read your user settings")
	assert.Empty(t, stderr)

	out, _ = reportCapturingStdout(t, agent.AgentNameClaudeCode, "notice-session-1", false)
	assert.Empty(t, out, "the notice shows once per session")

	out, _ = reportCapturingStdout(t, agent.AgentNameClaudeCode, "notice-session-2", false)
	assert.Contains(t, out, "could not read your user settings", "a new session sees it again")
}

// A set-up repository's lifecycle banner carries the notice instead.
func TestReportUnreadableUserSettings_SetUpLeavesItToTheBanner(t *testing.T) {
	setUserSettings(t, "{not json")
	out, stderr := reportCapturingStdout(t, agent.AgentNameClaudeCode, "notice-setup", true)
	assert.Empty(t, out)
	assert.Empty(t, stderr)
}

func TestReportUnreadableUserSettings_AgentWithoutBannerGetsStderr(t *testing.T) {
	setUserSettings(t, "{not json")
	out, stderr := reportCapturingStdout(t, agent.AgentNameCursor, "notice-cursor", false)
	assert.Empty(t, out)
	assert.Contains(t, stderr, "[entire] Entire could not read your user settings")
}

func TestReportUnreadableUserSettings_ReadableSettingsSayNothing(t *testing.T) {
	setUserSettings(t, `{"global_enable": true}`)
	out, stderr := reportCapturingStdout(t, agent.AgentNameClaudeCode, "notice-readable", false)
	assert.Empty(t, out)
	assert.Empty(t, stderr)
}
