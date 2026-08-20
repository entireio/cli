package checkpoint

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// prepareSubagentTranscript sanitizes a subagent transcript for storage and reports
// whether it is too large to store, mirroring what the session-transcript path does
// a few lines away in each store.
//
// Sanitize-before-redact is the same load-bearing order CLAUDE.md documents for the
// session transcript, and it matters most for exactly the agent that reaches this
// path most: Codex rollouts carry base64 encrypted_content — measured up to 20% of
// file bytes — which is bound to the originating session and cannot be replayed out
// of a checkpoint. Storing it is useless, and base64 is the pathological input for
// the redaction entropy layer, so redacting it first costs roughly twice as long
// (measured 1.31s vs 0.67s on a 3.3MB rollout).
//
// The size guard exists because this blob, unlike the session transcript, is neither
// chunked nor capped: redaction runs at roughly 220ms/MB, so an oversized rollout
// would sit in the subagent-stop hook for seconds. It measures the sanitized size,
// not the raw one — see below. Skipping is the honest outcome when even that is too
// large (there is no chunked form to fall back to), so it warns rather than failing
// the checkpoint, which still records the subagent's files and metadata.
//
// The agent type must be passed in, not detected: DetectAgentTypeFromContent only
// recognizes Gemini, so content-based detection would silently make this a no-op for
// Codex — the one agent that actually needs sanitizing.
func prepareSubagentTranscript(ctx context.Context, agentType types.AgentType, path string, content []byte) (prepared []byte, tooLarge bool) {
	// Sanitize first, then measure. The size that matters is what would be stored,
	// and sanitizing strips the bulk: Codex encrypted_content runs to ~20% of a
	// rollout's bytes. Measuring the raw input would drop a rollout that is oversized
	// only because of payloads about to be discarded. Sanitizing is cheap next to the
	// redaction this guard protects (~8ms/MB against ~220ms/MB), so paying it before
	// the decision costs little even when the answer is "skip".
	sanitized := SanitizeTranscriptForAgentType(agentType, content)
	if len(sanitized) > agent.MaxChunkSize {
		logging.Warn(ctx, "subagent transcript exceeds the blob size cap, storing checkpoint without it",
			slog.String("path", path),
			slog.Int("raw_bytes", len(content)),
			slog.Int("sanitized_bytes", len(sanitized)),
			slog.Int("cap", agent.MaxChunkSize))
		return nil, true
	}
	return sanitized, false
}
