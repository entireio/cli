package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below set environment variables, so none of them runs in parallel.

const adoptHomeSessionID = "adopt-across-homes"

// adoptHomeSession is a Claude Code session of sourceRepo whose transcript and
// one task transcript live in the source repository's project directory under
// home, which is not the active Claude home.
type adoptHomeSession struct {
	sourceRepo, home, transcript, task string
}

// newAdoptHomeSession isolates the agent homes, creates a source and a target
// repository, and saves an adoptHomeSession whose AgentHome is its home. It
// leaves the process in the target repository.
func newAdoptHomeSession(t *testing.T) adoptHomeSession {
	t.Helper()

	attachHomesEnv(t)
	s := adoptHomeSession{sourceRepo: setupAdoptRepo(t), home: canonicalDir(t)}
	project := filepath.Join(s.home, "projects", claudecode.SanitizePathForClaude(s.sourceRepo))
	s.transcript = filepath.Join(project, adoptHomeSessionID+".jsonl")
	s.task = filepath.Join(paths.SubagentsDir(project, adoptHomeSessionID), "agent-a1.jsonl")
	testutil.WriteFile(t, project, filepath.Base(s.transcript), attachHomeClaudeTranscript)

	lastInteraction := time.Now().Add(-1 * time.Minute)
	require.NoError(t, s.sourceStore().Save(context.Background(), &session.State{
		SessionID:           adoptHomeSessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		AgentHome:           s.home,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, s.sourceRepo),
		WorktreePath:        s.sourceRepo,
		TranscriptPath:      s.transcript,
		TaskRecords: []session.TaskRecord{
			{ToolUseID: "toolu_a1", AgentID: "a1", DeclaredTranscriptPath: s.task},
			// Under the home, but not this task's transcript in Claude's layout.
			{ToolUseID: "toolu_a2", AgentID: "a2", DeclaredTranscriptPath: filepath.Join(project, "other-session.jsonl")},
		},
	}))

	targetRepo := setupAdoptRepo(t)
	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	return s
}

func (s adoptHomeSession) sourceStore() *session.StateStore {
	return session.NewStateStoreWithDir(filepath.Join(s.sourceRepo, ".git", session.SessionStateDirName))
}

// updateSource applies fn to the saved source session state.
func (s adoptHomeSession) updateSource(t *testing.T, fn func(*session.State)) {
	t.Helper()

	state, err := s.sourceStore().Load(context.Background(), adoptHomeSessionID)
	require.NoError(t, err)
	fn(state)
	require.NoError(t, s.sourceStore().Save(context.Background(), state))
}

// activeTranscript writes a transcript for the session in the active Claude
// home's session directory for the source repository and returns its path.
func (s adoptHomeSession) activeTranscript(t *testing.T) string {
	t.Helper()

	claude, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	dir, err := claude.GetSessionDir(s.sourceRepo)
	require.NoError(t, err)
	testutil.WriteFile(t, dir, adoptHomeSessionID+".jsonl", attachHomeClaudeTranscript)
	return filepath.Join(dir, adoptHomeSessionID+".jsonl")
}

// adopt runs entire session adopt for the session and returns its output and
// error, and the state saved in the target repository.
func (s adoptHomeSession) adopt(t *testing.T) (string, *session.State, error) {
	t.Helper()

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, adoptHomeSessionID, adoptOptions{FromWorktree: s.sourceRepo, Force: true})
	targetStore, storeErr := session.NewStateStore(context.Background())
	require.NoError(t, storeErr)
	adopted, loadErr := targetStore.Load(context.Background(), adoptHomeSessionID)
	require.NoError(t, loadErr)
	return out.String(), adopted, err
}

func TestSessionAdopt_AcceptsTranscriptsUnderARecordedHome(t *testing.T) {
	s := newAdoptHomeSession(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeClaudeCode, s.home))

	out, adopted, err := s.adopt(t)

	require.NoError(t, err)
	require.NotNil(t, adopted)
	assert.Equal(t, s.transcript, adopted.TranscriptPath)
	assert.Equal(t, s.home, adopted.AgentHome)
	require.Len(t, adopted.TaskRecords, 2)
	assert.Equal(t, s.task, adopted.TaskRecords[0].DeclaredTranscriptPath, "the task's transcript under the recorded home is kept")
	assert.Empty(t, adopted.TaskRecords[1].DeclaredTranscriptPath, "Claude's task layout still applies under the recorded home")
	assert.Contains(t, out, "Dropped 1 subagent transcript path(s)")
}

func TestSessionAdopt_RejectsTranscriptsUnderAnUnrecordedHome(t *testing.T) {
	s := newAdoptHomeSession(t)

	_, adopted, err := s.adopt(t)

	require.Error(t, err)
	assert.Nil(t, adopted, "target state was written despite the refusal")
	require.ErrorIs(t, err, agent.ErrUntrustedAgentHome)
	assert.Contains(t, err.Error(), "not owned by a registered agent")
	for _, envVar := range agent.RelocationEnvVars() {
		assert.Contains(t, err.Error(), envVar)
	}
}

func TestSessionAdopt_RejectsAnotherProjectUnderATrustedHome(t *testing.T) {
	tests := []struct {
		name string
		// home returns the trusted home the state names.
		home func(t *testing.T, s adoptHomeSession) string
	}{
		{name: "recorded home", home: func(t *testing.T, s adoptHomeSession) string {
			t.Helper()
			require.NoError(t, agent.RememberAgentHome(agent.AgentTypeClaudeCode, s.home))
			return s.home
		}},
		{name: "active home", home: func(t *testing.T, _ adoptHomeSession) string {
			t.Helper()
			claude, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
			require.NoError(t, err)
			provider, ok := agent.AsHomeLayoutProvider(claude)
			require.True(t, ok)
			home, err := provider.SessionHome()
			require.NoError(t, err)
			return home
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newAdoptHomeSession(t)
			home := tt.home(t, s)
			// A predictably named file of another project in the same store.
			other := filepath.Join(home, "projects", "-home-user-private", "memory", "MEMORY.md")
			testutil.WriteFile(t, filepath.Dir(other), filepath.Base(other), "private\n")
			s.updateSource(t, func(state *session.State) {
				state.AgentHome = home
				state.TranscriptPath = other
				state.TaskRecords = nil
			})

			_, adopted, err := s.adopt(t)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "is outside the session directory for "+s.sourceRepo+", under both the session's agent home "+home+" and the active home")
			assert.NotContains(t, err.Error(), "CLAUDE_CONFIG_DIR", "the home is trusted, so a relocation variable would not help")
			assert.Nil(t, adopted)
		})
	}
}

func TestSessionAdopt_IgnoresARecordedHomeTheStateDoesNotName(t *testing.T) {
	s := newAdoptHomeSession(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeClaudeCode, s.home))
	s.updateSource(t, func(state *session.State) { state.AgentHome = "" })

	_, adopted, err := s.adopt(t)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not owned by a registered agent")
	assert.NotContains(t, err.Error(), "recorded agent home", "a state without AgentHome has no home to report on")
	assert.Nil(t, adopted)
}

func TestSessionAdopt_DropsAHomeThatDoesNotHoldTheTranscript(t *testing.T) {
	tests := []struct {
		name      string
		recorded  bool
		agentType bool
	}{
		{name: "untrusted home", agentType: true},
		{name: "trusted home", recorded: true, agentType: true},
		{name: "no agent type", recorded: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newAdoptHomeSession(t)
			if tt.recorded {
				require.NoError(t, agent.RememberAgentHome(agent.AgentTypeClaudeCode, s.home))
			}
			// The transcript is accepted the way it is without a home, from
			// the active home; the home the state names must not carry over.
			transcript := s.activeTranscript(t)
			s.updateSource(t, func(state *session.State) {
				state.TranscriptPath = transcript
				state.TaskRecords = nil
				if !tt.agentType {
					state.AgentType = ""
				}
			})

			_, adopted, err := s.adopt(t)

			require.NoError(t, err)
			require.NotNil(t, adopted)
			assert.Equal(t, transcript, adopted.TranscriptPath)
			assert.Equal(t, agent.AgentTypeClaudeCode, adopted.AgentType)
			assert.Empty(t, adopted.AgentHome)
		})
	}
}

func TestValidateAdoptSourceTranscript_CodexUnderARecordedHome(t *testing.T) {
	attachHomesEnv(t)
	home := canonicalDir(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, home))
	sourceRepo := t.TempDir()
	archived := filepath.Join(home, "archived_sessions", "rollout-2026-04-08T10-43-48-"+attachHomeCodexSessionID+".jsonl")
	child := filepath.Join(home, "sessions", "2026", "04", "08", "rollout-2026-04-08T10-44-00-child.jsonl")
	state := &session.State{
		SessionID:      attachHomeCodexSessionID,
		AgentType:      agent.AgentTypeCodex,
		AgentHome:      home,
		TranscriptPath: archived,
	}

	got, err := validateAdoptSourceTranscript(state, sourceRepo)

	require.NoError(t, err, "an archived rollout under a recorded Codex home is accepted")
	assert.Equal(t, home, got.path)
	require.NoError(t, validateAdoptTaskTranscript(state, got, "child", child, sourceRepo))
	assert.ErrorContains(t, validateAdoptTaskTranscript(state, got, "other", child, sourceRepo), "not the transcript of task")
}

func TestValidateAdoptSourceTranscript_HomeNotHoldingTheTranscriptVouchesForNoTask(t *testing.T) {
	attachHomesEnv(t)
	home := canonicalDir(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, home))
	codex, err := agent.GetByAgentType(agent.AgentTypeCodex)
	require.NoError(t, err)
	sourceRepo := t.TempDir()
	activeDir, err := codex.GetSessionDir(sourceRepo)
	require.NoError(t, err)
	state := &session.State{
		SessionID:      attachHomeCodexSessionID,
		AgentType:      agent.AgentTypeCodex,
		AgentHome:      home,
		TranscriptPath: filepath.Join(activeDir, "2026", "04", "08", "rollout-2026-04-08T10-43-48-"+attachHomeCodexSessionID+".jsonl"),
	}
	staleChild := filepath.Join(home, "sessions", "2026", "04", "08", "rollout-2026-04-08T10-44-00-child.jsonl")

	got, err := validateAdoptSourceTranscript(state, sourceRepo)

	require.NoError(t, err)
	assert.Empty(t, got.path, "a home that does not hold the transcript is not returned")
	assert.ErrorContains(t, validateAdoptTaskTranscript(state, got, "child", staleChild, sourceRepo), "not owned by a registered agent")
}

func TestValidateAdoptSourceTranscript_NoTranscriptKeepsNoHome(t *testing.T) {
	attachHomesEnv(t)
	home := canonicalDir(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, home))
	sourceRepo := t.TempDir()
	state := &session.State{SessionID: attachHomeCodexSessionID, AgentType: agent.AgentTypeCodex, AgentHome: home}
	child := filepath.Join(home, "sessions", "2026", "04", "08", "rollout-2026-04-08T10-44-00-child.jsonl")

	got, err := validateAdoptSourceTranscript(state, sourceRepo)

	require.NoError(t, err)
	assert.Empty(t, got.path, "a home with no transcript behind it is not returned")
	assert.ErrorContains(t, validateAdoptTaskTranscript(state, got, "child", child, sourceRepo), "not owned by a registered agent")
}

func TestValidateAdoptSourceTranscript_HomeWithoutTheWorktreesSessions(t *testing.T) {
	attachHomesEnv(t)
	t.Setenv("ENTIRE_TEST_PI_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	// Pi keeps its sessions outside its home: there is nowhere beneath a home
	// to check a transcript against.
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", canonicalDir(t))
	home := canonicalDir(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypePi, home))
	state := &session.State{
		SessionID:      "pi-session",
		AgentType:      agent.AgentTypePi,
		AgentHome:      home,
		TranscriptPath: filepath.Join(home, "sessions", "--repo--", "pi-session.jsonl"),
	}

	_, err := validateAdoptSourceTranscript(state, t.TempDir())

	require.ErrorIs(t, err, errAdoptHomeUnscoped)
	require.ErrorContains(t, err, "the session's agent home "+home+" cannot be checked: Pi keeps this worktree's sessions outside its active home")
	assert.NotContains(t, err.Error(), "refused")
	assert.NotContains(t, err.Error(), "rerun adopt", "the relocation variable is already set")
}
