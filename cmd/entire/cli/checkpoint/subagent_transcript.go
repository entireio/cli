package checkpoint

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
)

// prepareSubagentTranscript sanitizes a subagent transcript for storage, then
// (opt-in) externalizes inline base64 images before measuring whether the result
// is too large to store. This mirrors the sanitize -> externalize -> (caller
// redacts) pipeline the session-transcript path runs a few lines away in each
// store (see prepareTranscriptForStorage in strategy/manual_commit_condensation.go);
// redaction itself stays the caller's job so it can share the JSONL-vs-plain
// fallback already written there.
//
// Sanitize-before-anything is the same load-bearing order CLAUDE.md documents for
// the session transcript, and it matters most for exactly the agent that reaches
// this path most: Codex rollouts carry base64 encrypted_content — measured up to
// 20% of file bytes — which is bound to the originating session and cannot be
// replayed out of a checkpoint. Storing it is useless, and base64 is the
// pathological input for the redaction entropy layer, so redacting it first costs
// roughly twice as long (measured 1.31s vs 0.67s on a 3.3MB rollout).
//
// Externalize-before-redact matters here for the same reason it does on the
// session path: base64 image data is the other pathological input for the
// entropy layer, and large inline-image transcripts would otherwise blow the
// size guard below whole instead of shrinking to something storable.
//
// The size guard exists because this blob, unlike the session transcript, is
// neither chunked nor capped: redaction runs at roughly 220ms/MB, so an oversized
// rollout would sit in the subagent-stop hook for seconds. It measures the
// sanitized-and-externalized size, not the raw one — see below. Skipping is the
// honest outcome when even that is too large (there is no chunked form to fall
// back to), so it warns rather than failing the checkpoint, which still records
// the subagent's files and metadata.
//
// The agent type must be passed in, not detected: DetectAgentTypeFromContent only
// recognizes Gemini, so content-based detection would silently make this a no-op for
// Codex — the one agent that actually needs sanitizing.
func prepareSubagentTranscript(ctx context.Context, agentType types.AgentType, path string, content []byte) (prepared []byte, assets []TranscriptAsset, tooLarge bool) {
	// Sanitize first, then measure. The size that matters is what would be stored,
	// and sanitizing strips the bulk: Codex encrypted_content runs to ~20% of a
	// rollout's bytes. Measuring the raw input would drop a rollout that is oversized
	// only because of payloads about to be discarded. Sanitizing is cheap next to the
	// redaction this guard protects (~8ms/MB against ~220ms/MB), so paying it before
	// the decision costs little even when the answer is "skip".
	sanitized := SanitizeTranscriptForAgentType(agentType, content)

	externalized, extractedAssets := externalizeSubagentTranscriptImages(ctx, agentType, path, sanitized)

	if len(externalized) > agent.MaxChunkSize {
		logging.Warn(ctx, "subagent transcript exceeds the blob size cap, storing checkpoint without it",
			slog.String("path", path),
			slog.Int("raw_bytes", len(content)),
			slog.Int("sanitized_bytes", len(sanitized)),
			slog.Int("externalized_bytes", len(externalized)),
			slog.Int("cap", agent.MaxChunkSize))
		return nil, nil, true
	}
	return externalized, extractedAssets, false
}

// externalizeSubagentTranscriptImages runs the same opt-in image-externalization
// step the session path runs (see externalizeSessionImages in
// strategy/manual_commit_condensation.go), gated on the same
// settings.IsImageExternalizationEnabled flag for consistency. When disabled,
// unsupported for the agent, or on error, it returns the input unchanged with no
// assets — the transcript then flows through inline exactly as it did before this
// step existed.
func externalizeSubagentTranscriptImages(ctx context.Context, agentType types.AgentType, path string, transcript []byte) ([]byte, []TranscriptAsset) {
	if !settings.IsImageExternalizationEnabled(ctx) {
		return transcript, nil
	}
	codec := imageextract.CodecFor(agentType)
	if codec == nil {
		return transcript, nil
	}
	rewritten, extracted, err := codec.ExtractImages(transcript)
	if err != nil {
		logging.Warn(ctx, "subagent transcript image externalization failed; leaving transcript inline",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return transcript, nil
	}
	if len(extracted) == 0 {
		return transcript, nil
	}
	assets := make([]TranscriptAsset, len(extracted))
	for i, a := range extracted {
		assets[i] = TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data}
	}
	return rewritten, assets
}
