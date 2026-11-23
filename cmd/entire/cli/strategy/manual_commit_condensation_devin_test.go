package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

type stubPreparerAgent struct {
	agent.Agent

	prepareErr   error
	prepareCalls int
	onPrepare    func(transcriptPath string)
}

func (a *stubPreparerAgent) PrepareTranscript(_ context.Context, transcriptPath string) error {
	a.prepareCalls++
	if a.onPrepare != nil {
		a.onPrepare(transcriptPath)
	}
	return a.prepareErr
}

func devinCondensationFixture(t *testing.T, sessionID string, phase session.Phase, agentType types.AgentType) (*SessionState, *os.Root, string) {
	t.Helper()
	tmpDir := t.TempDir()
	livePath := filepath.Join(tmpDir, "live.jsonl")
	require.NoError(t, os.WriteFile(livePath, []byte("LIVE-BYTES\n"), 0o600))

	storedName, err := storedSessionFileName(sessionID, paths.TranscriptFileName)
	require.NoError(t, err)
	storedPath := filepath.Join(tmpDir, "stored", filepath.FromSlash(storedName))
	require.NoError(t, os.MkdirAll(filepath.Dir(storedPath), 0o750))
	require.NoError(t, os.WriteFile(storedPath, []byte("STORED-"+sessionID+"\n"), 0o600))

	root, err := os.OpenRoot(filepath.Join(tmpDir, "stored"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	state := &SessionState{
		SessionID:      sessionID,
		AgentType:      agentType,
		Phase:          phase,
		TranscriptPath: livePath,
	}
	return state, root, livePath
}

func TestCondensationTranscript_DevinPrepareFailureFailsClosed(t *testing.T) {
	t.Parallel()
	for _, phase := range []session.Phase{session.PhaseActive, session.PhaseIdle, session.PhaseEnded} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			state, root, _ := devinCondensationFixture(t, "sess-devin-"+string(phase), phase, devinAgentType)
			prepareErr := errors.New("devin transcript store unavailable")
			ag := &stubPreparerAgent{prepareErr: prepareErr}

			data, path, err := condensationTranscript(context.Background(), ag, state, root)

			require.ErrorIs(t, err, prepareErr)
			assert.Empty(t, data)
			assert.Empty(t, path)
			assert.Equal(t, 1, ag.prepareCalls)
		})
	}
}

func TestExtractSessionDataFrom_DevinPrepareFailureDegrades(t *testing.T) {
	t.Parallel()
	state, root, _ := devinCondensationFixture(t, "sess-devin-extract", session.PhaseIdle, devinAgentType)
	s := NewManualCommitStrategy()
	ag := &stubPreparerAgent{prepareErr: errors.New("devin store gone")}

	data, err := s.extractSessionDataFrom(context.Background(), ag, state, root)

	require.NoError(t, err)
	require.NotNil(t, data)
	assert.Equal(t, "unavailable", data.CaptureStatus)
	assert.Equal(t, "prepare_failed", data.CaptureReason)
	assert.Empty(t, data.Transcript)
	assert.Equal(t, 1, ag.prepareCalls)
}

func TestCondensationTranscript_DevinPrepareRefreshesBeforeRead(t *testing.T) {
	t.Parallel()
	for _, phase := range []session.Phase{session.PhaseActive, session.PhaseIdle, session.PhaseEnded} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			state, root, livePath := devinCondensationFixture(t, "sess-devin-fresh-"+string(phase), phase, devinAgentType)
			ag := &stubPreparerAgent{
				onPrepare: func(string) {
					require.NoError(t, os.WriteFile(livePath, []byte("FRESH-BYTES\n"), 0o600))
				},
			}

			data, path, err := condensationTranscript(context.Background(), ag, state, root)

			require.NoError(t, err)
			assert.Equal(t, livePath, path)
			assert.Equal(t, "FRESH-BYTES\n", string(data))
			assert.Equal(t, 1, ag.prepareCalls)
		})
	}
}

func TestCondensationTranscript_NonDevinPrepareFailureStaysBestEffort(t *testing.T) {
	t.Parallel()
	for _, phase := range []session.Phase{session.PhaseActive, session.PhaseIdle, session.PhaseEnded} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			state, root, _ := devinCondensationFixture(t, "sess-mock-"+string(phase), phase, types.AgentType("Mock Agent"))
			ag := &stubPreparerAgent{prepareErr: errors.New("transient fetch failure")}

			data, _, err := condensationTranscript(context.Background(), ag, state, root)

			require.NoError(t, err)
			assert.Equal(t, "LIVE-BYTES\n", string(data))
			if phase == session.PhaseActive {
				assert.Equal(t, 1, ag.prepareCalls)
			} else {
				assert.Zero(t, ag.prepareCalls)
			}
		})
	}
}

func TestCondensationTranscript_NonDevinFallsBackToStored(t *testing.T) {
	t.Parallel()
	state, root, livePath := devinCondensationFixture(t, "sess-mock-stored", session.PhaseIdle, types.AgentType("Mock Agent"))
	require.NoError(t, os.Remove(livePath))
	ag := &stubPreparerAgent{prepareErr: errors.New("transient fetch failure")}

	data, path, err := condensationTranscript(context.Background(), ag, state, root)

	require.NoError(t, err)
	assert.Empty(t, path)
	assert.Equal(t, "STORED-sess-mock-stored\n", string(data))
	assert.Zero(t, ag.prepareCalls)
}

func TestCondensationTranscript_DevinRequiresTranscriptPath(t *testing.T) {
	t.Parallel()
	state, root, _ := devinCondensationFixture(t, "sess-devin-nopath", session.PhaseIdle, devinAgentType)
	state.TranscriptPath = ""
	ag := &stubPreparerAgent{}

	data, path, err := condensationTranscript(context.Background(), ag, state, root)

	require.Error(t, err)
	assert.Empty(t, data)
	assert.Empty(t, path)
	assert.Zero(t, ag.prepareCalls)
}

func TestCondensationTranscript_DevinRequiresPreparer(t *testing.T) {
	t.Parallel()
	for name, ag := range map[string]agent.Agent{
		"nilagent":  nil,
		"noprepare": plainAgent{},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state, root, _ := devinCondensationFixture(t, "sess-devin-"+name, session.PhaseIdle, devinAgentType)

			data, path, err := condensationTranscript(context.Background(), ag, state, root)

			require.Error(t, err)
			assert.Empty(t, data)
			assert.Empty(t, path)
		})
	}
}

func TestCondensationTranscript_DevinRequiresResolvablePath(t *testing.T) {
	t.Parallel()
	state, root, livePath := devinCondensationFixture(t, "sess-devin-noresolve", session.PhaseIdle, devinAgentType)
	require.NoError(t, os.Remove(livePath))
	ag := &stubPreparerAgent{}

	data, path, err := condensationTranscript(context.Background(), ag, state, root)

	require.Error(t, err)
	assert.Empty(t, data)
	assert.Empty(t, path)
	assert.Equal(t, 1, ag.prepareCalls)
}

func TestCondensationTranscript_DevinRejectsUnreadableLive(t *testing.T) {
	t.Parallel()
	state, root, livePath := devinCondensationFixture(t, "sess-devin-unreadable", session.PhaseIdle, devinAgentType)
	require.NoError(t, os.Remove(livePath))
	require.NoError(t, os.Mkdir(livePath, 0o750))
	ag := &stubPreparerAgent{}

	data, path, err := condensationTranscript(context.Background(), ag, state, root)

	require.Error(t, err)
	assert.Empty(t, data)
	assert.Empty(t, path)
	assert.Equal(t, 1, ag.prepareCalls)
}

func TestCondensationTranscript_DevinRejectsEmptyLive(t *testing.T) {
	t.Parallel()
	state, root, livePath := devinCondensationFixture(t, "sess-devin-empty", session.PhaseIdle, devinAgentType)
	require.NoError(t, os.WriteFile(livePath, []byte{}, 0o600))
	ag := &stubPreparerAgent{}

	data, path, err := condensationTranscript(context.Background(), ag, state, root)

	require.Error(t, err)
	assert.Empty(t, data)
	assert.Empty(t, path)
	assert.Equal(t, 1, ag.prepareCalls)
}
