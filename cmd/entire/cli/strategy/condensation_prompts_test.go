package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// The prompt ladder's last rung must work from the transcript BYTES being
// checkpointed. When condensation fell back to the stored turn-end copy because
// the live path could not be read, a re-read of the path found nothing and the
// checkpoint carried a full transcript with no prompt.
func TestResolveCondensationPrompts_UsesTranscriptBytesWhenLivePathIsGone(t *testing.T) {
	t.Parallel()
	ag := antigravity.NewAntigravityAgent()
	transcript := []byte(`{"type":"USER_INPUT","content":"<USER_REQUEST>\nfirst ask\n</USER_REQUEST>"}
{"type":"PLANNER_RESPONSE"}
{"type":"USER_INPUT","content":"<USER_REQUEST>\nadd another\n</USER_REQUEST>"}
`)
	missing := filepath.Join(t.TempDir(), "gone.jsonl")

	got := resolveCondensationPrompts(context.Background(), ag, transcript, missing, 2)
	require.Equal(t, []string{"add another"}, got)

	// Without bytes in hand the rung degrades to the path-based read, which
	// finds nothing here — the behaviour this test exists to stop relying on.
	require.Nil(t, resolveCondensationPrompts(context.Background(), ag, nil, missing, 2))
}

// TestExtractSessionData_ResolvesPromptsWithEmptyTranscript pins the prompt
// ladder OUTSIDE the transcript gate. A LateTranscriptWriter routinely condenses
// an empty placeholder mid-turn; while the ladder sat inside the transcript
// check that produced a checkpoint with no prompt from ANY source — not even
// the prompt.txt staged on disk — and silenced logCondensationPrompts with it.
//
// Uses t.Chdir — do NOT add t.Parallel().
func TestExtractSessionData_ResolvesPromptsWithEmptyTranscript(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	const sessionID = "20260924-agy-empty-transcript"
	metadataDir := filepath.Join(dir, paths.SessionMetadataDirFromSessionID(sessionID))
	require.NoError(t, os.MkdirAll(metadataDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, paths.PromptFileName), []byte("the prompt that must survive"), 0o600))
	// The empty placeholder PrepareTranscript materialises when Stop beats
	// agy's flush.
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, nil, 0o600))

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:      sessionID,
		AgentType:      agent.AgentTypeAntigravity,
		TranscriptPath: transcriptPath,
		StepCount:      1,
	}
	data, err := s.extractSessionData(context.Background(), antigravity.NewAntigravityAgent(), state)
	require.NoError(t, err)
	require.Empty(t, data.Transcript, "sanity: this is the empty-transcript case")
	require.Equal(t, []string{"the prompt that must survive"}, data.Prompts,
		"prompt resolution must not depend on transcript content")
}
