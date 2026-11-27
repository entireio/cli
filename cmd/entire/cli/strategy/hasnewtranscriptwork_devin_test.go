package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type transcriptWorkAgent struct {
	agent.Agent

	agentType     types.AgentType
	prepareErr    error
	prepareCalls  int
	positionCalls int
	position      int
	positionErr   error
	modifiedFiles []string
}

func (a *transcriptWorkAgent) Name() types.AgentName { return "transcript-work-test" }
func (a *transcriptWorkAgent) Type() types.AgentType { return a.agentType }
func (a *transcriptWorkAgent) Description() string   { return "transcript work test agent" }
func (a *transcriptWorkAgent) DeclaredCapabilities() agent.DeclaredCaps {
	return agent.DeclaredCaps{TranscriptAnalyzer: true, TranscriptPreparer: true}
}
func (a *transcriptWorkAgent) PrepareTranscript(_ context.Context, _ string) error {
	a.prepareCalls++
	return a.prepareErr
}
func (a *transcriptWorkAgent) GetTranscriptPosition(_ string) (int, error) {
	a.positionCalls++
	return a.position, a.positionErr
}
func (a *transcriptWorkAgent) ExtractModifiedFilesFromOffset(_ context.Context, _ string, _ int) ([]string, int, error) {
	return a.modifiedFiles, a.position, a.positionErr
}

func transcriptWorkState(t *testing.T, agentType types.AgentType) *SessionState {
	t.Helper()
	transcriptPath := filepath.Join(t.TempDir(), "stale-transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("{\"type\":\"human\"}\n"), 0o600))
	state := &SessionState{
		SessionID:                 "sess-work-" + string(agentType),
		AgentType:                 agentType,
		Phase:                     session.PhaseActive,
		TranscriptPath:            transcriptPath,
		CheckpointTranscriptStart: 0,
	}
	return state
}

func registerTranscriptWorkAgent(t *testing.T, ag agent.Agent) {
	t.Helper()
	restore := agent.SnapshotRegistryForTesting()
	agent.Register("transcript-work-test", func() agent.Agent { return ag })
	t.Cleanup(restore)
}

func TestHasNewTranscriptWork_DevinPrepareErrorReturnsFalse(t *testing.T) {
	state := transcriptWorkState(t, devinAgentType)
	ag := &transcriptWorkAgent{agentType: devinAgentType, prepareErr: errors.New("devin store unavailable"), position: 10}
	registerTranscriptWorkAgent(t, ag)

	got := NewManualCommitStrategy().hasNewTranscriptWork(context.Background(), state)

	assert.False(t, got, "a failed Devin prepare must not fall through to the stale transcript position")
	assert.Equal(t, 1, ag.prepareCalls)
	assert.Zero(t, ag.positionCalls, "GetTranscriptPosition must not run after a Devin prepare failure")
}

func TestHasNewTranscriptWork_DevinPrepareSuccessReturnsTrue(t *testing.T) {
	state := transcriptWorkState(t, devinAgentType)
	ag := &transcriptWorkAgent{agentType: devinAgentType, position: 10}
	registerTranscriptWorkAgent(t, ag)

	got := NewManualCommitStrategy().hasNewTranscriptWork(context.Background(), state)

	assert.True(t, got)
	assert.Equal(t, 1, ag.prepareCalls)
	assert.Equal(t, 1, ag.positionCalls)
}

func TestHasNewTranscriptWork_NonDevinPrepareErrorKeepsPriorPath(t *testing.T) {
	state := transcriptWorkState(t, types.AgentType("Mock Work Agent"))
	ag := &transcriptWorkAgent{agentType: types.AgentType("Mock Work Agent"), prepareErr: errors.New("transient"), position: 10}
	registerTranscriptWorkAgent(t, ag)

	got := NewManualCommitStrategy().hasNewTranscriptWork(context.Background(), state)

	assert.True(t, got, "non-Devin agents keep the best-effort prepare path")
	assert.Equal(t, 1, ag.prepareCalls)
	assert.Equal(t, 1, ag.positionCalls)
}
