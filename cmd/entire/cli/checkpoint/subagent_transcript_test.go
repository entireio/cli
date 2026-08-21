package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// TestPrepareSubagentTranscript_SanitizesCodexRollout pins sanitize-before-redact on
// the subagent path. Codex rollouts carry base64 encrypted_content that is bound to
// the originating session and cannot be replayed out of a checkpoint, so storing it
// is useless — and because base64 is the pathological input for the redaction entropy
// layer, redacting it instead of dropping it roughly doubles the time this hook
// spends (measured 1.31s vs 0.67s on a 3.3MB rollout).
func TestPrepareSubagentTranscript_SanitizesCodexRollout(t *testing.T) {
	t.Parallel()

	rollout := `{"type":"session_meta","payload":{"id":"abc"}}
{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"QUFBQUFBQUFBQUFBQUFBQUFBQUE="}}
`
	got, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/x.jsonl", []byte(rollout))
	require.False(t, tooLarge)
	require.NotContains(t, string(got), "encrypted_content",
		"Codex encrypted reasoning must be stripped before the transcript is stored")
	require.Contains(t, string(got), "session_meta", "the rest of the rollout must survive")
}

// TestPrepareSubagentTranscript_SkipsOversizeTranscript covers the size guard. Unlike
// the session transcript this blob is neither chunked nor capped, and redaction runs
// at roughly 220ms/MB, so without a guard an oversized rollout parks the
// subagent-stop hook for seconds.
func TestPrepareSubagentTranscript_SkipsOversizeTranscript(t *testing.T) {
	t.Parallel()

	oversize := make([]byte, agent.MaxChunkSize+1)
	for i := range oversize {
		oversize[i] = 'a'
	}

	got, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/big.jsonl", oversize)
	require.True(t, tooLarge, "a transcript past the blob cap must be skipped, not redacted")
	require.Nil(t, got)
}

// TestPrepareSubagentTranscript_LeavesOtherAgentsAlone is the companion guard: only
// agents with something to sanitize are touched, so Claude Code / Droid subagent
// transcripts pass through byte-for-byte.
func TestPrepareSubagentTranscript_LeavesOtherAgentsAlone(t *testing.T) {
	t.Parallel()

	claude := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n"
	got, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeClaudeCode, "/x/agent-a1.jsonl", []byte(claude))
	require.False(t, tooLarge)
	require.Equal(t, strings.TrimSpace(claude), strings.TrimSpace(string(got)))
}

// TestPrepareSubagentTranscript_MeasuresSanitizedSize is the guard's ordering
// contract: a Codex rollout that is over the cap only because of encrypted_content
// must still be stored, because that payload is stripped before anything is written.
// Measuring the raw bytes instead would throw away a transcript that fits.
func TestPrepareSubagentTranscript_MeasuresSanitizedSize(t *testing.T) {
	t.Parallel()

	// One reasoning line whose ciphertext alone pushes the raw file past the cap.
	ciphertext := strings.Repeat("A", agent.MaxChunkSize+1024)
	rollout := `{"type":"session_meta","payload":{"id":"abc"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"` + ciphertext + `"}}` + "\n"
	require.Greater(t, len(rollout), agent.MaxChunkSize, "fixture must be oversized before sanitizing")

	got, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/big.jsonl", []byte(rollout))

	require.False(t, tooLarge,
		"a rollout that fits once encrypted_content is stripped must not be dropped")
	require.LessOrEqual(t, len(got), agent.MaxChunkSize)
	require.NotContains(t, string(got), "encrypted_content")
	require.Contains(t, string(got), "session_meta", "the rest of the rollout must survive")
}
