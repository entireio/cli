package cli

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestFormatCheckpointOutput_CaptureStatusRows(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV")

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.Metadata{
			CheckpointID:            cpID,
			SessionID:               "sess-1",
			Strategy:                "manual-commit",
			Agent:                   "Devin",
			TranscriptCaptureStatus: "unavailable",
			TranscriptCaptureReason: "prepare_failed",
		},
	}

	out := formatCheckpointOutput(context.Background(), &checkpoint.CheckpointSummary{}, content, cpID, nil, checkpoint.Author{}, false, false, io.Discard)
	assert.Contains(t, out, "Transcript: capture unavailable (prepare_failed) — conversation was not recorded for this commit")

	content.Metadata.TranscriptCaptureStatus = "repaired"
	content.Metadata.TranscriptCaptureReason = ""
	out = formatCheckpointOutput(context.Background(), &checkpoint.CheckpointSummary{}, content, cpID, nil, checkpoint.Author{}, false, false, io.Discard)
	assert.Contains(t, out, "Transcript: captured after commit (repaired)")
	assert.NotContains(t, out, "capture unavailable")

	content.Metadata.TranscriptCaptureStatus = ""
	out = formatCheckpointOutput(context.Background(), &checkpoint.CheckpointSummary{}, content, cpID, nil, checkpoint.Author{}, false, false, io.Discard)
	assert.NotContains(t, out, "Transcript: capture")
	assert.NotContains(t, out, "Transcript: captured after commit")
}

func TestFormatCheckpointOutput_CaptureStatusWithTranscript(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.Metadata{
			CheckpointID:            cpID,
			SessionID:               "sess-1",
			Strategy:                "manual-commit",
			Agent:                   "Devin",
			TranscriptCaptureStatus: "unavailable",
			TranscriptCaptureReason: "snapshot_empty",
		},
		Transcript: []byte("{\"type\":\"human\",\"message\":{\"content\":\"hi\"}}\n"),
	}
	out := formatCheckpointOutput(context.Background(), &checkpoint.CheckpointSummary{}, content, cpID, nil, checkpoint.Author{}, true, false, io.Discard)
	require.Contains(t, out, "Transcript: capture unavailable (snapshot_empty)")
}
