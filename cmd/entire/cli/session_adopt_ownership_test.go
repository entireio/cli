package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestAdoptRecordedHome_PreservesSessionIdentityAcrossProjects(t *testing.T) {
	for _, agentType := range []types.AgentType{agent.AgentTypeClaudeCode, agent.AgentTypeFactoryAIDroid, agent.AgentTypePi} {
		t.Run(string(agentType), func(t *testing.T) {
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			for _, name := range agent.RelocationEnvVars() {
				t.Setenv(name, "")
			}
			ag, err := agent.GetByAgentType(agentType)
			require.NoError(t, err)
			provider, ok := ag.(agent.WorktreeSessionDirProvider)
			require.True(t, ok)
			home, sourceRoot, otherRoot := t.TempDir(), t.TempDir(), t.TempDir()
			require.NoError(t, agent.RememberAgentHome(agentType, home))
			sourceDir := provider.SessionDirUnder(home, sourceRoot)
			otherDir := provider.SessionDirUnder(home, otherRoot)
			testutil.WriteFile(t, sourceDir, "session.jsonl", "{}\n")
			testutil.WriteFile(t, otherDir, "session.jsonl", "private\n")
			other := filepath.Join(otherDir, "session.jsonl")
			state := &session.State{SessionID: "session", AgentType: agentType, AgentHome: home, TranscriptPath: other}
			require.NoError(t, validateAdoptSourceTranscript(state, sourceRoot))
			state.TranscriptPath = filepath.Join(sourceDir, "session.jsonl")
			state.TaskRecords = []session.TaskRecord{{AgentID: "session", DeclaredTranscriptPath: other}, {AgentID: "session", DeclaredTranscriptPath: state.TranscriptPath}}
			require.NoError(t, validateAdoptSourceTranscript(state, sourceRoot))
			require.Empty(t, state.TaskRecords[0].DeclaredTranscriptPath)
			require.Equal(t, state.TranscriptPath, state.TaskRecords[1].DeclaredTranscriptPath)
			foreignChild := filepath.Join(sourceDir, "another-session", "subagents", "agent-child.jsonl")
			state.TaskRecords = []session.TaskRecord{{AgentID: "child", DeclaredTranscriptPath: foreignChild}}
			require.NoError(t, validateAdoptSourceTranscript(state, sourceRoot))
			require.Empty(t, state.TaskRecords[0].DeclaredTranscriptPath)
			state.TranscriptPath = other
			state.SessionID = "different"
			require.Error(t, validateAdoptSourceTranscript(state, sourceRoot))
		})
	}
}

func TestAdoptRecordedHome_RejectsMismatchedSessionPaths(t *testing.T) {
	const id = "selected-session"
	for _, tc := range []struct {
		kind types.AgentType
		name string
	}{
		{agent.AgentTypeClaudeCode, "projects/original/" + id + ".jsonl"},
		{agent.AgentTypeFactoryAIDroid, ".factory/sessions/original/" + id + ".jsonl"},
		{agent.AgentTypePi, "sessions/original/2026-10-03_" + id + ".jsonl"},
		{agent.AgentTypeCodex, "sessions/2026/10/03/rollout-2026-10-03-" + id + ".jsonl"},
		{agent.AgentTypeCodex, "archived_sessions/2026/10/03/rollout-2026-10-03-" + id + ".jsonl"},
		{agent.AgentTypeCopilotCLI, "session-state/" + id + "/events.jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			for _, name := range agent.RelocationEnvVars() {
				t.Setenv(name, "")
			}
			home := t.TempDir()
			require.NoError(t, agent.RememberAgentHome(tc.kind, home))
			testutil.WriteFile(t, home, tc.name, "{}\n")
			state := &session.State{SessionID: id, AgentType: tc.kind, AgentHome: home, TranscriptPath: filepath.Join(home, filepath.FromSlash(tc.name))}
			require.NoError(t, validateAdoptSourceTranscript(state, t.TempDir()))
			// Layout/identity checks allow the next write; discovery separately
			// requires a readable transcript. Timestamped names must work too.
			require.NoError(t, os.Remove(state.TranscriptPath))
			require.NoError(t, validateAdoptSourceTranscript(state, t.TempDir()))
			for _, different := range []string{"unrelated-session", "../selected-session", "*", ""} {
				state.SessionID = different
				require.Error(t, validateAdoptSourceTranscript(state, t.TempDir()))
			}
		})
	}
}

func TestAdoptRetargetedHomeAlias(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	isolateAgentHomesRegistry(t)
	for _, name := range agent.RelocationEnvVars() {
		t.Setenv(name, "")
	}
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	first, second := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "active-home")
	require.NoError(t, os.Symlink(first, alias))
	t.Setenv("CLAUDE_CONFIG_DIR", alias)
	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	const id = "selected-session"
	project := adoptClaudeProject(t, first, t.TempDir())
	testutil.WriteFile(t, project, id+".jsonl", "{}\n")
	rel, err := filepath.Rel(first, filepath.Join(project, id+".jsonl"))
	require.NoError(t, err)
	require.NoError(t, agent.RememberAgentHome(ag.Type(), alias))
	state := &session.State{SessionID: id, AgentType: ag.Type(), AgentHome: alias, TranscriptPath: filepath.Join(alias, rel)}
	require.NoError(t, validateAdoptSourceTranscript(state, t.TempDir()))
	canonicalHome := state.AgentHome
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(second, alias))
	require.NoError(t, agent.RememberAgentHome(ag.Type(), alias))
	require.NoError(t, validateAdoptSourceTranscript(state, t.TempDir()))
	found, home, err := searchTranscriptInProjectDirs(id, ag)
	require.NoError(t, err)
	require.Equal(t, state.TranscriptPath, found)
	require.Equal(t, canonicalHome, home)
}
