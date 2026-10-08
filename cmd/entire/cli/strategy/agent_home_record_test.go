package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below set environment variables, so none of them runs in parallel.

// canonicalTempDir returns a fresh directory's canonical path.
func canonicalTempDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// relocateClaudeHome points Claude Code's home at a fresh directory, isolates
// the per-user agent home registry, and returns the home.
func relocateClaudeHome(t *testing.T) string {
	t.Helper()

	home := canonicalTempDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	return home
}

func TestUpdateSessionAgentHome(t *testing.T) {
	home := relocateClaudeHome(t)
	other := t.TempDir()
	inHome := filepath.Join(home, "projects", "repo", "session.jsonl")
	inOther := filepath.Join(other, "projects", "repo", "session.jsonl")

	tests := []struct {
		name         string
		agentType    types.AgentType
		stored       string
		transcript   string
		wantHome     string
		wantRemember string
	}{
		{name: "transcript in the active home", agentType: agent.AgentTypeClaudeCode, transcript: inHome,
			wantHome: home, wantRemember: home},
		{name: "active home replaces another stored home", agentType: agent.AgentTypeClaudeCode, stored: other, transcript: inHome,
			wantHome: home, wantRemember: home},
		{name: "stored home still holding the transcript is kept", agentType: agent.AgentTypeClaudeCode, stored: other, transcript: inOther,
			wantHome: other},
		{name: "transcript outside every home", agentType: agent.AgentTypeClaudeCode, stored: other,
			transcript: filepath.Join(t.TempDir(), "session.jsonl")},
		{name: "transcript beside the store, not in it", agentType: agent.AgentTypeClaudeCode,
			transcript: filepath.Join(home, "projects-old", "session.jsonl")},
		{name: "transcript escaping the store with dot-dot", agentType: agent.AgentTypeClaudeCode,
			transcript: filepath.Join(home, "projects") + "/../../escape.jsonl"},
		{name: "no transcript keeps the stored home", agentType: agent.AgentTypeClaudeCode, stored: other,
			wantHome: other},
		{name: "agent without a home layout", agentType: agent.AgentTypeCursor, stored: other, transcript: inOther},
		{name: "unknown agent", stored: other, transcript: inOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &SessionState{AgentType: tt.agentType, AgentHome: tt.stored, TranscriptPath: tt.transcript}

			remember := updateSessionAgentHome(t.Context(), state)

			assert.Equal(t, tt.wantHome, state.AgentHome, "AgentHome")
			assert.Equal(t, tt.wantRemember, remember, "home to remember")
		})
	}
}

func TestUpdateSessionAgentHome_SymlinkedActiveHome(t *testing.T) {
	relocateClaudeHome(t)
	home := canonicalTempDir(t)
	link := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", link)

	// The agent may report its transcript through the link or through the
	// directory it points to; the home is stored in the spelling that holds it.
	for _, tt := range []struct{ name, home string }{
		{name: "through the link", home: link},
		{name: "through the target", home: home},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := &SessionState{
				AgentType:      agent.AgentTypeClaudeCode,
				TranscriptPath: filepath.Join(tt.home, "projects", "repo", "session.jsonl"),
			}

			remember := updateSessionAgentHome(t.Context(), state)

			assert.Equal(t, tt.home, state.AgentHome)
			assert.Equal(t, tt.home, remember)
		})
	}
}

func TestInitializeSession_RecordsTheAgentHome(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "init.txt", "init\n")
	testutil.GitAdd(t, dir, "init.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	home := relocateClaudeHome(t)
	ctx := t.Context()
	s := &ManualCommitStrategy{}
	const sessionID = "record-agent-home"

	turn := func(transcript string) *SessionState {
		t.Helper()
		require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, transcript, "prompt", ""))
		state, err := s.loadSessionState(ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, state)
		return state
	}
	known := func() []string {
		t.Helper()
		homes, err := agent.KnownAgentHomes(agent.AgentTypeClaudeCode)
		require.NoError(t, err)
		return homes
	}

	// Session start records the home in the state and the registry.
	state := turn(filepath.Join(home, "projects", "repo", sessionID+".jsonl"))
	assert.Equal(t, home, state.AgentHome)
	assert.Equal(t, []string{home}, known())

	// A turn start from a second home records it as the most recent.
	second := canonicalTempDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", second)
	state = turn(filepath.Join(second, "projects", "repo", sessionID+".jsonl"))
	assert.Equal(t, second, state.AgentHome)
	assert.Equal(t, []string{second, home}, known())

	// A turn whose transcript lies outside every home clears it.
	state = turn(filepath.Join(t.TempDir(), sessionID+".jsonl"))
	assert.Empty(t, state.AgentHome)
	assert.Equal(t, []string{second, home}, known())
}
