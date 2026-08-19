package checkpoint

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// claudeSubagentImageLine returns a Claude-shaped subagent transcript line
// embedding one inline base64 image, plus the base64 string for assertions.
func claudeSubagentImageLine(payload string) (line, b64 string) {
	b64 = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + payload))
	line = `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}` + "\n"
	return line, b64
}

// codexSubagentImageLine returns a Codex-shaped subagent rollout line embedding
// one inline base64 image as a data-URI, plus the base64 string for assertions.
func codexSubagentImageLine(payload string) (line, b64 string) {
	b64 = base64.StdEncoding.EncodeToString([]byte("\xFF\xD8\xFF" + payload))
	line = `{"type":"response_item","payload":{"type":"message","role":"user","content":[` +
		`{"type":"input_image","image_url":"data:image/jpeg;base64,` + b64 + `"}` +
		`]}}` + "\n"
	return line, b64
}

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
	got, _, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/x.jsonl", []byte(rollout))
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

	got, assets, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/big.jsonl", oversize)
	require.True(t, tooLarge, "a transcript past the blob cap must be skipped, not redacted")
	require.Nil(t, got)
	require.Nil(t, assets)
}

// TestPrepareSubagentTranscript_LeavesOtherAgentsAlone is the companion guard: only
// agents with something to sanitize are touched, so Claude Code / Droid subagent
// transcripts pass through byte-for-byte.
func TestPrepareSubagentTranscript_LeavesOtherAgentsAlone(t *testing.T) {
	t.Parallel()

	claude := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n"
	got, _, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeClaudeCode, "/x/agent-a1.jsonl", []byte(claude))
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

	got, _, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/big.jsonl", []byte(rollout))

	require.False(t, tooLarge,
		"a rollout that fits once encrypted_content is stripped must not be dropped")
	require.LessOrEqual(t, len(got), agent.MaxChunkSize)
	require.NotContains(t, string(got), "encrypted_content")
	require.Contains(t, string(got), "session_meta", "the rest of the rollout must survive")
}

// TestPrepareSubagentTranscript_ExternalizesClaudeImages_WhenEnabled covers the
// bug fix: a subagent transcript with an inline image must go through the same
// sanitize -> externalize pipeline the session path uses, not straight from
// sanitize to the size cap. These tests mutate process-global settings state
// (env var / cwd), so they cannot run in parallel with each other.
func TestPrepareSubagentTranscript_ExternalizesClaudeImages_WhenEnabled(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	line, b64 := claudeSubagentImageLine(strings.Repeat("claude-subagent-image-bytes", 4))

	got, assets, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeClaudeCode, "/x/agent-a1.jsonl", []byte(line))

	require.False(t, tooLarge)
	require.Len(t, assets, 1, "the inline image must be lifted into an asset")
	require.NotContains(t, string(got), b64, "image bytes must be gone from the transcript")
	require.Contains(t, string(got), "entire-asset:assets/", "a placeholder must be left behind")
	require.Less(t, len(got), len(line), "externalizing the image must shrink the stored transcript")
	require.Equal(t, assets[0].Data, mustDecodeB64(t, b64))
}

// TestPrepareSubagentTranscript_ExternalizesCodexImages_WhenEnabled is the Codex
// analog: Codex subagent rollouts carry images as data-URIs, and the fix
// description calls out that undoing this bug also protects Codex input_image
// base64 from the redaction entropy layer (the caller redacts next).
func TestPrepareSubagentTranscript_ExternalizesCodexImages_WhenEnabled(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	line, b64 := codexSubagentImageLine(strings.Repeat("codex-subagent-image-bytes", 4))

	got, assets, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeCodex, "/rollouts/agent-a1.jsonl", []byte(line))

	require.False(t, tooLarge)
	require.Len(t, assets, 1, "the inline image must be lifted into an asset")
	require.NotContains(t, string(got), b64, "image bytes must be gone from the transcript")
	require.Contains(t, string(got), "entire-asset:assets/", "a placeholder must be left behind")
	require.Equal(t, assets[0].Data, mustDecodeB64(t, b64))
}

// TestPrepareSubagentTranscript_ImageExternalization_DisabledLeavesInline pins
// the feature-flag gate: with externalization off (the default), the transcript
// must flow through exactly as it did before this fix — no extraction, no
// assets, byte-identical output (modulo sanitize, which is a no-op for Claude
// Code content with no agent-state fields to strip).
func TestPrepareSubagentTranscript_ImageExternalization_DisabledLeavesInline(t *testing.T) {
	t.Chdir(t.TempDir()) // isolate settings; externalization defaults off

	line, b64 := claudeSubagentImageLine(strings.Repeat("disabled-subagent-image-bytes", 4))

	got, assets, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeClaudeCode, "/x/agent-a1.jsonl", []byte(line))

	require.False(t, tooLarge)
	require.Nil(t, assets, "no assets should be produced when externalization is disabled")
	require.Contains(t, string(got), b64, "image must stay inline when externalization is disabled")
	require.Equal(t, strings.TrimSpace(line), strings.TrimSpace(string(got)))
}

// TestPrepareSubagentTranscript_OversizedEvenAfterExternalize_StillTooLarge
// covers requirement 4: the size guard must run on the externalized
// (post-shrink) transcript, and a transcript that is still too large even after
// its image is lifted out must still be dropped — with the now-orphaned assets
// discarded too, since the transcript that would reference them is never
// stored.
func TestPrepareSubagentTranscript_OversizedEvenAfterExternalize_StillTooLarge(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	// Padding alone exceeds the cap, so the transcript stays oversized no matter
	// how much the image externalization shrinks it.
	padding := strings.Repeat("a", agent.MaxChunkSize+1024)
	b64 := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("oversized-after-externalize", 4)))
	line := `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"` + padding + `"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}` + "\n"
	require.Greater(t, len(line), agent.MaxChunkSize, "fixture must be oversized before externalizing")

	got, assets, tooLarge := prepareSubagentTranscript(context.Background(), agent.AgentTypeClaudeCode, "/x/agent-a1.jsonl", []byte(line))

	require.True(t, tooLarge, "text padding alone exceeds the cap even after the image is externalized")
	require.Nil(t, got)
	require.Nil(t, assets, "assets must not be returned for a transcript that is dropped")
}

func mustDecodeB64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return raw
}
