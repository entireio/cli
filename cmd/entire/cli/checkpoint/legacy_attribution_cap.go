package checkpoint

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// MaxLegacyPromptAttributionsBytes bounds the prompt_attributions a per-session
// metadata.json may carry forward.
//
// The field is dead data: no CLI version still computes, reads or sets it, and
// Metadata.LegacyPromptAttributions exists only so that rewriting an old
// checkpoint does not strip what entire.io still reads. But the carry-forward
// has no exit: writeSessionToSubdirectory reads the field out of the previous
// blob and writes it into the new one on every overwrite of a session slot, so
// an oversized value an older CLI recorded is re-committed for the life of the
// session rather than ageing out.
//
// It is also the one field whose size scales with the working tree rather than
// with the session — CLI versions before v0.10.1 recorded every file of every
// nested git checkout on every prompt, and produced 70MB and 106MB
// metadata.json blobs that made entire/checkpoints/v1 unpushable to GitHub,
// whose per-blob hard limit is 100 MiB. Measured over this repo's v1 history
// (11,929 distinct metadata.json blobs), the largest prompt_attributions is
// 1.73 MiB and only 11 blobs exceed 64 KiB, so 4 MiB clears every healthy
// record by a wide margin and fires only on the pathological shape above.
//
// The sibling initial_attribution is deliberately not capped: it is one
// snapshot per session rather than one per prompt, and over the same history
// its largest value is 254 bytes.
const MaxLegacyPromptAttributionsBytes = 4 << 20

// CapLegacyPromptAttributions returns raw unchanged when it fits under
// MaxLegacyPromptAttributionsBytes, and nil — dropping the field from the
// written metadata — when it does not.
//
// Dropping one opaque field is the cheaper loss: a blob over GitHub's 100 MiB
// limit cannot be pushed at all, which strands every checkpoint behind it on
// the branch, transcript and summary included. The drop is logged so the
// omission is explainable from .entire/logs afterwards, which is why ctx must
// be the caller's real context and never context.Background() — logging falls
// back to slog.Default() when ctx carries no logger, and nothing calls
// slog.SetDefault in production, so a background context would print to hook
// stderr and never reach the log.
func CapLegacyPromptAttributions(ctx context.Context, raw json.RawMessage, sessionID string) json.RawMessage {
	if len(raw) <= MaxLegacyPromptAttributionsBytes {
		return raw
	}
	logging.Warn(logging.WithComponent(ctx, "checkpoint"),
		"dropping oversized legacy prompt_attributions from session metadata",
		slog.Int("bytes", len(raw)),
		slog.Int("cap_bytes", MaxLegacyPromptAttributionsBytes),
		slog.String("session_id", sessionID))
	return nil
}
