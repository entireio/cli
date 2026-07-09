package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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

// costPtr is a small helper for constructing optional cost values in tests.
func costPtr(v float64) *float64 { return &v }

// The per-model breakdown must be omitted from the wire form when empty, so
// legacy readers and backends that never asked for it see no field at all.
func TestModelUsage_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(Metadata{})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if strings.Contains(string(b), "model_usage") {
		t.Fatalf("empty ModelUsage should be omitted, got: %s", b)
	}

	b, err = json.Marshal(CheckpointSummary{})
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(b), "model_usage") {
		t.Fatalf("empty summary ModelUsage should be omitted, got: %s", b)
	}
}

// The backend contract is a nested shape: each entry has a "model" string and a
// "token_usage" object. Assert the exact raw JSON keys so a field rename can't
// silently break ingestion.
func TestModelUsage_NestedShapeExact(t *testing.T) {
	t.Parallel()

	meta := Metadata{
		ModelUsage: []types.ModelUsage{
			{
				Model: "claude-opus-4-8",
				TokenUsage: types.TokenUsage{
					InputTokens:  1200000,
					OutputTokens: 3400,
					CostUSD:      costPtr(0.42),
					CostSource:   types.CostSourceEstimated,
				},
			},
		},
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Decode into a generic map to assert the exact nested key names.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	list, ok := raw["model_usage"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("model_usage not a 1-element array: %v", raw["model_usage"])
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("model_usage[0] not an object: %v", list[0])
	}
	if entry["model"] != "claude-opus-4-8" {
		t.Fatalf(`entry["model"] = %v, want "claude-opus-4-8"`, entry["model"])
	}
	tu, ok := entry["token_usage"].(map[string]any)
	if !ok {
		t.Fatalf(`entry["token_usage"] not a nested object: %v`, entry["token_usage"])
	}
	if in, ok := tu["input_tokens"].(float64); !ok || in != 1200000 {
		t.Fatalf("token_usage.input_tokens = %v, want 1200000", tu["input_tokens"])
	}
	if cost, ok := tu["cost_usd"].(float64); !ok || cost != 0.42 {
		t.Fatalf("token_usage.cost_usd = %v, want 0.42", tu["cost_usd"])
	}

	// Full round-trip preserves the typed value.
	var got Metadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if len(got.ModelUsage) != 1 || got.ModelUsage[0].Model != "claude-opus-4-8" ||
		got.ModelUsage[0].TokenUsage.InputTokens != 1200000 {
		t.Fatalf("round-trip mismatch: %+v", got.ModelUsage)
	}
}

// Legacy metadata written before this field existed has no "model_usage" key;
// decoding must leave ModelUsage nil (not panic, not a zero-length non-nil slice).
func TestModelUsage_LegacyJSONNil(t *testing.T) {
	t.Parallel()

	legacy := `{"strategy":"manual-commit","token_usage":{"input_tokens":5}}`
	var got Metadata
	if err := json.Unmarshal([]byte(legacy), &got); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if got.ModelUsage != nil {
		t.Fatalf("legacy ModelUsage = %v, want nil", got.ModelUsage)
	}
}
