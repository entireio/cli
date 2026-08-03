package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImportedFlagsOnSummaryAndInfo(t *testing.T) {
	t.Parallel()
	if !(CheckpointSummary{Imported: true}).Imported {
		t.Fatal("CheckpointSummary.Imported not settable")
	}
	if !(CheckpointInfo{Imported: true}).Imported {
		t.Fatal("CheckpointInfo.Imported not settable")
	}
}

func TestGetCompactTranscriptStart(t *testing.T) {
	t.Parallel()

	// nil pointer = legacy checkpoint whose transcript.jsonl holds only the delta.
	if offset, ok := (Metadata{}).GetCompactTranscriptStart(); ok || offset != 0 {
		t.Fatalf("nil: got (%d, %v), want (0, false)", offset, ok)
	}

	// Pointer to 0 = full compact file whose first checkpoint starts at line 0.
	// Must be distinguishable from the nil (legacy) case above.
	zero := 0
	if offset, ok := (Metadata{CompactTranscriptStart: &zero}).GetCompactTranscriptStart(); !ok || offset != 0 {
		t.Fatalf("&0: got (%d, %v), want (0, true)", offset, ok)
	}

	five := 5
	if offset, ok := (Metadata{CompactTranscriptStart: &five}).GetCompactTranscriptStart(); !ok || offset != 5 {
		t.Fatalf("&5: got (%d, %v), want (5, true)", offset, ok)
	}
}

func TestCompactTranscriptStart_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	// nil is omitted entirely, so legacy readers see no field.
	b, err := json.Marshal(Metadata{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "compact_transcript_start") {
		t.Fatalf("nil pointer should be omitted, got: %s", b)
	}

	// A set value (including 0) round-trips and stays distinguishable from absent.
	zero := 0
	b, err = json.Marshal(Metadata{CompactTranscriptStart: &zero})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"compact_transcript_start":0`) {
		t.Fatalf("expected explicit 0 in JSON, got: %s", b)
	}

	var got Metadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if offset, ok := got.GetCompactTranscriptStart(); !ok || offset != 0 {
		t.Fatalf("round-trip: got (%d, %v), want (0, true)", offset, ok)
	}
}
