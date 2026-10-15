package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
)

// These change directory (t.Chdir), so they cannot run in parallel.

func mustGetAgent(t *testing.T, name types.AgentName) agent.Agent {
	t.Helper()
	ag, err := agent.Get(name)
	require.NoError(t, err)
	return ag
}

// installCurrentClaudeHooks installs today's Claude Code hooks in the current
// repo and returns the settings file path.
func installCurrentClaudeHooks(t *testing.T, dir string) string {
	t.Helper()
	_, err := (&claudecode.ClaudeCodeAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	return filepath.Join(dir, claudeDirName, claudecode.ClaudeSettingsFileName)
}

func TestMissingHooksBannerSuffix_MissingSkillHook(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	settingsPath := installCurrentClaudeHooks(t, dir)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings map[string]map[string][]map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	settings["hooks"]["PostToolUse"] = slices.DeleteFunc(settings["hooks"]["PostToolUse"], func(m map[string]any) bool {
		return m["matcher"] == "Skill"
	})
	data, err = json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, data, 0o600))

	require.Equal(t, missingHooksWarning, missingHooksBannerSuffix(context.Background(), mustGetAgent(t, agent.AgentNameClaudeCode)))
}

// A command shape this CLI does not write (as a newer CLI might) makes status
// report the hooks outdated, but must not reach the banner: following it would
// rewrite the teammate's newer config back to this CLI's shape.
func TestMissingHooksBannerSuffix_StaleCommandStaysSilent(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	settingsPath := installCurrentClaudeHooks(t, dir)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	ctx := context.Background()
	require.NotContains(t, OutdatedHookAgents(ctx), agent.AgentNameClaudeCode, "fresh install should read as current")
	newer := strings.Replace(string(data), "claude-code stop'", "claude-code stop --newer-flag'", 1)
	require.NotEqual(t, string(data), newer, "fixture should have changed the Stop command")
	require.NoError(t, os.WriteFile(settingsPath, []byte(newer), 0o600))

	require.Contains(t, OutdatedHookAgents(ctx), agent.AgentNameClaudeCode, "status should still report it")
	require.Empty(t, missingHooksBannerSuffix(ctx, mustGetAgent(t, agent.AgentNameClaudeCode)))
}

func TestMissingHooksBannerSuffix_Current(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	installCurrentClaudeHooks(t, dir)

	require.Empty(t, missingHooksBannerSuffix(context.Background(), mustGetAgent(t, agent.AgentNameClaudeCode)))
}

// No Entire Stop hook means Entire isn't installed for Claude Code here, so
// there is nothing to warn about.
func TestMissingHooksBannerSuffix_Absent(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	require.Empty(t, missingHooksBannerSuffix(context.Background(), mustGetAgent(t, agent.AgentNameClaudeCode)))
}

// OpenCode's check compares the whole generated plugin, so a teammate on a
// different CLI version reads as outdated. It must not reach the banner.
func TestMissingHooksBannerSuffix_OpenCodeOutdatedStaysSilent(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	ctx := context.Background()
	ag := mustGetAgent(t, agent.AgentNameOpenCode)
	hs, ok := agent.AsHookSupport(ag)
	require.True(t, ok)
	_, err := hs.InstallHooks(ctx, false)
	require.NoError(t, err)

	plugin := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(plugin)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(plugin, append(data, "// edited by another CLI version\n"...), 0o600))
	require.Contains(t, OutdatedHookAgents(ctx), agent.AgentNameOpenCode, "fixture must read as outdated")

	require.Empty(t, missingHooksBannerSuffix(ctx, ag))
}

func TestHandleLifecycleSessionStart_MissingHooksWarning(t *testing.T) {
	t.Chdir(setupGitRepoForPhaseTest(t))
	writeOutdatedClaudeHooks(t)
	ag := newMockHookResponseAgent()
	ag.name = agent.AgentNameClaudeCode

	require.NoError(t, handleLifecycleSessionStart(context.Background(), ag, &agent.Event{
		Type: agent.SessionStart, SessionID: "test-missing-hooks-warning", Timestamp: time.Now(),
	}))
	require.Contains(t, ag.lastMessage, "Run 'entire agent add claude-code' to add them.")
}
