package checkpoint

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// MaxPromptAttributionsBytes bounds the prompt_attributions field a per-session
// metadata.json may carry. The field is a diagnostic record of the per-prompt
// line counts that fed the attribution summary: nothing in the CLI reads it back,
// and entire-api's ingest explicitly salvages a metadata.json whose tail it
// occupies. It is also the one field whose size scales with the working tree
// rather than with the session — CLI versions before v0.10.1 recorded every file
// of every nested git checkout on every prompt, and produced 70MB and 106MB
// metadata.json blobs that made entire/checkpoints/v1 unpushable to GitHub
// (100 MiB blob limit). A healthy session's record is tens of kilobytes, so
// 4 MiB is two orders of magnitude of headroom before the field is dropped.
const MaxPromptAttributionsBytes = 4 << 20

// CapPromptAttributions returns raw unchanged when it fits under
// MaxPromptAttributionsBytes, and nil — dropping the field from the written
// metadata — when it does not. The attribution summary itself is computed before
// this point and is unaffected; only the diagnostic input is withheld, and the
// drop is logged so the omission is explainable from .entire/logs.
func CapPromptAttributions(ctx context.Context, raw json.RawMessage, sessionID string) json.RawMessage {
	if len(raw) <= MaxPromptAttributionsBytes {
		return raw
	}
	logging.Warn(logging.WithComponent(ctx, "checkpoint"),
		"dropping oversized prompt_attributions from session metadata",
		slog.Int("bytes", len(raw)),
		slog.Int("cap_bytes", MaxPromptAttributionsBytes),
		slog.String("session_id", sessionID))
	return nil
}
