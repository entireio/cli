package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// TestExtractSessionDataFromLiveTranscript_EmptyTranscriptDegrades verifies the
// first-turn condensation path tolerates an empty live transcript instead of
// hard-erroring. Antigravity writes its transcript AFTER the Stop hook, so a
// mid-turn commit on the first turn condenses while the transcript is still an
// empty placeholder (created by PrepareTranscript). Erroring here happens AFTER
// prepare-commit-msg stamped the Entire-Checkpoint trailer — the commit would
// permanently reference a checkpoint that was never written. The shadow-branch
// path already tolerates empty transcripts; the live path must degrade the same
// way: empty transcript content, FilesTouched preserved from hook capture.
func TestExtractSessionDataFromLiveTranscript_EmptyTranscriptDegrades(t *testing.T) {
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

	data, err := s.extractSessionDataFromLiveTranscript(context.Background(), state)
	require.NoError(t, err, "empty live transcript must degrade, not fail — a hard error strands the already-stamped Entire-Checkpoint trailer")
	require.NotNil(t, data)
	require.Equal(t, []string{"docs/blue.md"}, data.FilesTouched, "hook-captured files must survive an empty transcript")
	require.Empty(t, data.Transcript)
}
