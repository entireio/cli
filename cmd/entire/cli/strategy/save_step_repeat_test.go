package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// saveStepRepeatFixture is a repo with one tracked file and a SaveStep helper
// that records a step for it with the given turn identity.
type saveStepRepeatFixture struct {
	t         *testing.T
	dir       string
	sessionID string
	s         *ManualCommitStrategy
}

func newSaveStepRepeatFixture(t *testing.T) *saveStepRepeatFixture {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "test.txt", "v0")
	testutil.GitAdd(t, dir, "test.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	f := &saveStepRepeatFixture{t: t, dir: dir, sessionID: "save-step-repeat", s: &ManualCommitStrategy{}}
	metadataDirAbs := filepath.Join(dir, ".entire", "metadata", f.sessionID)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(`{"type":"human","message":{"content":"test"}}`+"\n"), 0o644))
	return f
}

func (f *saveStepRepeatFixture) save(nonce string, transcriptSize int, identifier string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, "test.txt"), []byte("changed by "+nonce+identifier), 0o644))
	require.NoError(f.t, f.s.SaveStep(context.Background(), StepContext{
		SessionID:                f.sessionID,
		MetadataDir:              ".entire/metadata/" + f.sessionID,
		ModifiedFiles:            []string{"test.txt"},
		AgentType:                agent.AgentTypeClaudeCode,
		StepTranscriptIdentifier: identifier,
		TokenUsage:               &agent.TokenUsage{InputTokens: 100, OutputTokens: 10},
		TurnNonce:                nonce,
		TranscriptSize:           transcriptSize,
	}))
}

func (f *saveStepRepeatFixture) state() *SessionState {
	f.t.Helper()
	state, err := f.s.loadSessionState(context.Background(), f.sessionID)
	require.NoError(f.t, err)
	require.NotNil(f.t, state)
	return state
}

func requireStepTokens(t *testing.T, state *SessionState, steps, sessionInput, windowInput int) {
	t.Helper()
	require.Equal(t, steps, state.StepCount, "step count")
	require.NotNil(t, state.TokenUsage)
	require.Equal(t, sessionInput, state.TokenUsage.InputTokens, "session total")
	require.NotNil(t, state.CheckpointTokenUsage)
	require.Equal(t, windowInput, state.CheckpointTokenUsage.InputTokens, "checkpoint window")
}

func TestSaveStep_RepeatedTurnNonceCountsOnce(t *testing.T) {
	f := newSaveStepRepeatFixture(t)
	f.save("turn-a", 500, "uuid-a")
	f.save("turn-a", 500, "uuid-a")
	requireStepTokens(t, f.state(), 1, 100, 100)
}

// A TurnStart that times out on the session lock writes its pre-prompt nonce
// but never rotates the session's TurnID. Its turn must still count.
func TestSaveStep_NewTurnNonceCountsEvenWithUnrotatedTurnID(t *testing.T) {
	f := newSaveStepRepeatFixture(t)
	f.save("turn-a", 500, "uuid-a")
	turnID := f.state().TurnID
	f.save("turn-b", 900, "uuid-b")
	state := f.state()
	require.Equal(t, turnID, state.TurnID, "PRECONDITION: TurnID not rotated")
	requireStepTokens(t, state, 2, 200, 200)
}

// A Stop without a pre-prompt nonce (it was consumed by an earlier Stop, or
// the turn had no TurnStart) is a repeat only if the transcript did not grow.
func TestSaveStep_WithoutNonceMatchesOnTranscriptSize(t *testing.T) {
	f := newSaveStepRepeatFixture(t)
	f.save("turn-a", 500, "uuid-a")
	f.save("", 500, "") // repeat whose pre-prompt state is gone
	requireStepTokens(t, f.state(), 1, 100, 100)

	f.save("", 900, "") // a new turn without TurnStart grew the transcript
	requireStepTokens(t, f.state(), 2, 200, 200)
}

func TestSaveStep_UnknownTurnIdentityAlwaysCounts(t *testing.T) {
	f := newSaveStepRepeatFixture(t)
	f.save("", 0, "")
	f.save("", 0, "")
	requireStepTokens(t, f.state(), 2, 200, 200)
}

// A commit between a Stop and its repeat condenses the window. The repeat's
// files are then new pending work and need a step, but its tokens were
// already counted and condensed, and the window's start identifier must stay.
func TestSaveStep_RepeatAfterCondensationCountsStepNotTokens(t *testing.T) {
	f := newSaveStepRepeatFixture(t)
	f.save("turn-a", 500, "uuid-a")
	require.NoError(t, MutateSessionState(context.Background(), f.sessionID, func(st *SessionState) error {
		st.StepCount = 0
		st.CheckpointTokenUsage = nil
		st.TranscriptIdentifierAtStart = "uuid-a"
		return nil
	}))

	f.save("", 500, "")
	state := f.state()
	require.Equal(t, 1, state.StepCount, "the repeat's files need a pending step")
	require.Equal(t, 100, state.TokenUsage.InputTokens, "the turn's tokens are not re-added")
	require.Nil(t, state.CheckpointTokenUsage, "the condensed window gets no tokens back")
	require.Equal(t, "uuid-a", state.TranscriptIdentifierAtStart, "a repeat must not reset the window start")
}
