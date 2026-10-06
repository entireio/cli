package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
)

// TestExtractSessionData_EmptyTranscriptDegradesForLateWriters verifies the
// first-turn condensation path tolerates an empty live transcript instead of
// hard-erroring. Antigravity writes its transcript AFTER the Stop hook, so a
// mid-turn commit on the first turn condenses while the transcript is still an
// empty placeholder (created by PrepareTranscript). Erroring here happens AFTER
// prepare-commit-msg stamped the Entire-Checkpoint trailer — the commit would
// permanently reference a checkpoint that was never written. It must degrade:
// empty transcript content, FilesTouched preserved from hook capture.
func TestExtractSessionData_EmptyTranscriptDegradesForLateWriters(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript_full.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, nil, 0o600)) // empty placeholder

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:      "agy-empty-live-test",
		AgentType:      agent.AgentTypeAntigravity,
		TranscriptPath: transcriptPath,
		FilesTouched:   []string{"docs/blue.md"},
	}

	data, err := s.extractSessionData(context.Background(), mustAgent(t, state.AgentType), state)
	require.NoError(t, err, "empty live transcript must degrade, not fail — a hard error strands the already-stamped Entire-Checkpoint trailer")
	require.NotNil(t, data)
	require.Equal(t, []string{"docs/blue.md"}, data.FilesTouched, "hook-captured files must survive an empty transcript")
	require.Empty(t, data.Transcript)
}

// TestExtractSessionData_EmptyTranscriptErrorsForOtherAgents
// pins the inverse: for agents that do NOT write their transcript after the
// Stop hook, an empty live transcript is a transient race and must error so
// the failed condensation leaves session state untouched and the next commit
// retries with the populated transcript.
func TestExtractSessionData_EmptyTranscriptErrorsForOtherAgents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, nil, 0o600))

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:      "claude-empty-live-test",
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: transcriptPath,
		FilesTouched:   []string{"a.txt"},
	}

	_, err := s.extractSessionData(context.Background(), mustAgent(t, state.AgentType), state)
	require.Error(t, err, "non-late-flush agents must keep the error/retry invariant on an empty live transcript")
}

// TestExtractSessionData_EmptyTranscriptDegradesAfterATurnEndStep pins the
// upgrade/relocation case: a session with a recorded turn-end step had a
// transcript at that Stop, so an unreadable one now is not a race that a retry
// fixes. Condensation must degrade to a files/prompt-only checkpoint instead of
// failing (and, for doctor's path, clearing the state).
func TestExtractSessionData_EmptyTranscriptDegradesAfterATurnEndStep(t *testing.T) {
	t.Parallel()

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:      "claude-moved-transcript-test",
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: filepath.Join(t.TempDir(), "gone.jsonl"),
		FilesTouched:   []string{"a.txt"},
		StepCount:      1,
	}

	data, err := s.extractSessionData(context.Background(), mustAgent(t, state.AgentType), state)
	require.NoError(t, err)
	require.Empty(t, data.Transcript)
	require.Equal(t, []string{"a.txt"}, data.FilesTouched)
}

func mustAgent(t *testing.T, agentType types.AgentType) agent.Agent {
	t.Helper()
	ag, err := agent.GetByAgentType(agentType)
	require.NoError(t, err)
	return ag
}
