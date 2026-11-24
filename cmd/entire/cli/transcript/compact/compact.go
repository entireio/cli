// Package compact provides the CLI's redaction-aware transcript conversion
// boundary and condensed transcript view.
package compact

import (
	"github.com/entireio/cli/redact"
	transcriptlib "github.com/entireio/cli/transcript"
)

// MetadataFields provides metadata fields written to every output line.
type MetadataFields = transcriptlib.Options

// Compact converts a full.jsonl transcript into the Entire Transcript Format.
// The input must be pre-redacted (via redact.JSONLBytes or
// redact.AlreadyRedacted for trusted sources).
func Compact(redacted redact.RedactedBytes, opts MetadataFields) ([]byte, error) {
	return transcriptlib.Convert(redacted.Bytes(), opts) //nolint:wrapcheck // compatibility wrapper preserves converter errors unchanged
}

// FullWithBoundary converts the full transcript and returns the compact-output
// line index at which this checkpoint's data begins. See transcriptlib.ConvertWithBoundary
// for the per-format offsets and inclusion rounding at streaming boundaries.
func FullWithBoundary(redacted redact.RedactedBytes, opts MetadataFields) ([]byte, int, error) {
	return transcriptlib.ConvertWithBoundary(redacted.Bytes(), opts) //nolint:wrapcheck // compatibility wrapper preserves converter errors unchanged
}
