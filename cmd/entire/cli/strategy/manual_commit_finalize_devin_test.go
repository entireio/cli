package strategy

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

type finalizeDevinAgent struct {
	agent.Agent

	prepareErr      error
	prepareCalls    int
	freshTranscript []byte
}

func (a *finalizeDevinAgent) Name() types.AgentName     { return "devin-finalize-test" }
func (a *finalizeDevinAgent) Type() types.AgentType     { return devinAgentType }
func (a *finalizeDevinAgent) Description() string       { return "Devin finalize test agent" }

func (a *finalizeDevinAgent) PrepareTranscript(_ context.Context, transcriptPath string) error {
	a.prepareCalls++
	if a.prepareErr != nil {
		return a.prepareErr
	}
	if a.freshTranscript != nil {
		return os.WriteFile(transcriptPath, a.freshTranscript, 0o600)
	}
	return nil
}

func registerFinalizeDevinAgent(t *testing.T, ag agent.Agent) {
	t.Helper()
	restore := agent.SnapshotRegistryForTesting()
	agent.Register("devin-finalize-test", func() agent.Agent { return ag })
	t.Cleanup(restore)
}

func devinFinalizeState(f *finalizeFixture) {
	f.state.AgentType = devinAgentType
}

func TestFinalizeAllTurnCheckpoints_DevinPrepareFailureIsDegradation(t *testing.T) {
	f := newFinalizeFixture(t)
	t.Cleanup(osroot.ResetShared)
	devinFinalizeState(f)
	turnIDs := append([]string(nil), f.state.TurnCheckpointIDs...)

	ag := &finalizeDevinAgent{prepareErr: errors.New("devin store unavailable")}
	registerFinalizeDevinAgent(t, ag)
	s := NewManualCommitStrategy()
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }

	errCount := s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute))
	require.Equal(t, 1, errCount)
	require.Equal(t, 1, ag.prepareCalls)
	require.NotNil(t, f.state.CaptureDegradedAt)
	require.Equal(t, turnIDs, f.state.TurnCheckpointIDs)
	f.requireTranscripts(t, 0)

	ag.prepareErr = nil
	ag.freshTranscript = []byte("{\"type\":\"human\",\"message\":{\"content\":\"FRESH-DEVIN-FINALIZE\"}}\n")
	require.Zero(t, s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute)))
	require.Empty(t, f.state.TurnCheckpointIDs)
	require.Equal(t, 2, ag.prepareCalls)
	f.restoreUnfetched(t)
	for _, checkpointID := range f.checkpointIDs {
		content, err := f.store.ReadSessionContent(context.Background(), checkpointID, 0)
		require.NoError(t, err)
		require.JSONEq(t, "{\"type\":\"human\",\"message\":{\"content\":\"FRESH-DEVIN-FINALIZE\"}}", string(content.Transcript))
	}
}

func TestFinalizeAllTurnCheckpoints_DevinUnknownAgentIsDegradation(t *testing.T) {
	f := newFinalizeFixture(t)
	t.Cleanup(osroot.ResetShared)
	devinFinalizeState(f)
	turnIDs := append([]string(nil), f.state.TurnCheckpointIDs...)

	restore := agent.SnapshotRegistryForTesting()
	t.Cleanup(restore)

	s := NewManualCommitStrategy()
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }

	errCount := s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute))
	require.Equal(t, 1, errCount)
	require.NotNil(t, f.state.CaptureDegradedAt)
	require.Equal(t, turnIDs, f.state.TurnCheckpointIDs)
	f.requireTranscripts(t, 0)
}

func TestFinalizeAllTurnCheckpoints_DevinEmptyTranscriptIsDegradation(t *testing.T) {
	f := newFinalizeFixture(t)
	t.Cleanup(osroot.ResetShared)
	devinFinalizeState(f)
	turnIDs := append([]string(nil), f.state.TurnCheckpointIDs...)

	ag := &finalizeDevinAgent{freshTranscript: []byte{}}
	registerFinalizeDevinAgent(t, ag)
	s := NewManualCommitStrategy()
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }

	errCount := s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute))
	require.Equal(t, 1, errCount)
	require.Equal(t, 1, ag.prepareCalls)
	require.NotNil(t, f.state.CaptureDegradedAt)
	require.Equal(t, turnIDs, f.state.TurnCheckpointIDs)
	f.requireTranscripts(t, 0)
}
