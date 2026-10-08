package claudecode

import "testing"

func TestExtractModel_MostRecentAssistantMessageWins(t *testing.T) {
	t.Parallel()
	// message.model is the clean, hook-consistent identifier; the most recent
	// assistant message reflects a mid-session model switch.
	transcript := `{"type":"system","subtype":"init","session_id":"s","model":"claude-opus-4-8[1m]"}
{"type":"assistant","message":{"model":"claude-opus-4-8","id":"m1","role":"assistant","content":[]}}
{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"model":"claude-sonnet-5","id":"m2","role":"assistant","content":[]}}`

	model, err := (&ClaudeCodeAgent{}).ExtractModel([]byte(transcript))
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "claude-sonnet-5" {
		t.Errorf("expected most recent assistant model %q, got %q", "claude-sonnet-5", model)
	}
}

func TestExtractModel_SkipsSyntheticPlaceholder(t *testing.T) {
	t.Parallel()
	// Claude Code sets message.model to "<synthetic>" on API-error assistant
	// entries. That placeholder must not replace the last genuine model.
	transcript := `{"type":"assistant","message":{"model":"claude-opus-4-8","id":"m1","role":"assistant","content":[]}}
{"type":"assistant","message":{"model":"<synthetic>","id":"m2","role":"assistant","content":[]}}`

	model, err := (&ClaudeCodeAgent{}).ExtractModel([]byte(transcript))
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("expected last genuine model %q, got %q", "claude-opus-4-8", model)
	}
}

func TestExtractModel_FallsBackToSystemInit(t *testing.T) {
	t.Parallel()
	// No assistant message yet (very short/early transcript): recover the model
	// from the system init line rather than reporting empty.
	transcript := `{"type":"system","subtype":"init","session_id":"s","model":"claude-opus-4-8[1m]"}
{"type":"user","message":{"role":"user","content":"hi"}}`

	model, err := (&ClaudeCodeAgent{}).ExtractModel([]byte(transcript))
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "claude-opus-4-8[1m]" {
		t.Errorf("expected system init model %q, got %q", "claude-opus-4-8[1m]", model)
	}
}

func TestExtractModel_EmptyTranscript(t *testing.T) {
	t.Parallel()
	model, err := (&ClaudeCodeAgent{}).ExtractModel(nil)
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "" {
		t.Errorf("expected empty model for empty transcript, got %q", model)
	}
}

func TestExtractModel_NoModelField(t *testing.T) {
	t.Parallel()
	// Assistant messages without a model field, and no system init model.
	transcript := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[]}}
{"type":"user","message":{"role":"user","content":"hi"}}`

	model, err := (&ClaudeCodeAgent{}).ExtractModel([]byte(transcript))
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "" {
		t.Errorf("expected empty model when none present, got %q", model)
	}
}

func TestExtractModel_IgnoresNonInitSystemEnvelope(t *testing.T) {
	t.Parallel()
	// A "system" line that is not the init envelope must not be mistaken for the
	// init-model fallback, even if it happens to carry a "model" field.
	transcript := `{"type":"system","subtype":"compact_boundary","model":"stale-model"}
{"type":"user","message":{"role":"user","content":"hi"}}`

	model, err := (&ClaudeCodeAgent{}).ExtractModel([]byte(transcript))
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "" {
		t.Errorf("expected empty model for non-init system envelope, got %q", model)
	}
}

func TestExtractModel_IgnoresMalformedLines(t *testing.T) {
	t.Parallel()
	// A corrupt/partial line (e.g. from an incompletely-flushed transcript) must
	// not abort extraction of the surrounding valid lines.
	transcript := `{"type":"system","subtype":"init","model":"claude-opus-4-8[1m]"}
{not valid json
{"type":"assistant","message":{"model":"claude-opus-4-8","id":"m1","role":"assistant","content":[]}}`

	model, err := (&ClaudeCodeAgent{}).ExtractModel([]byte(transcript))
	if err != nil {
		t.Fatalf("ExtractModel returned error: %v", err)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("expected %q despite malformed line, got %q", "claude-opus-4-8", model)
	}
}
