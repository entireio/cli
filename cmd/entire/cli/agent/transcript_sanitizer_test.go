package agent

import (
	"bytes"
	"testing"
)

// mockSanitizingAgent implements TranscriptSanitizer by dropping any line that
// contains "SECRETSTATE", standing in for Codex's encrypted_content stripping.
type mockSanitizingAgent struct {
	mockBaseAgent

	calls int
	// returnNil forces the contract-violating nil return so we can prove
	// SanitizeTranscriptForStorage fails safe rather than dropping the session.
	returnNil bool
}

func (m *mockSanitizingAgent) SanitizeTranscriptForStorage(data []byte) []byte {
	m.calls++
	if m.returnNil {
		return nil
	}
	var kept [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.Contains(line, []byte("SECRETSTATE")) {
			continue
		}
		kept = append(kept, line)
	}
	return bytes.Join(kept, []byte("\n"))
}

func TestSanitizeTranscriptForStorage_AppliesAgentSanitizer(t *testing.T) {
	t.Parallel()

	ag := &mockSanitizingAgent{}
	in := []byte("keep me\nSECRETSTATE=abc\nkeep me too")

	got := SanitizeTranscriptForStorage(ag, in)

	if bytes.Contains(got, []byte("SECRETSTATE")) {
		t.Errorf("sanitizer not applied, got %q", got)
	}
	if !bytes.Contains(got, []byte("keep me")) || !bytes.Contains(got, []byte("keep me too")) {
		t.Errorf("sanitizer dropped real content, got %q", got)
	}
	if ag.calls != 1 {
		t.Errorf("expected 1 sanitizer call, got %d", ag.calls)
	}
}

func TestSanitizeTranscriptForStorage_Idempotent(t *testing.T) {
	t.Parallel()

	// Idempotency is what lets every storage path call this without knowing
	// whether an upstream path already sanitized.
	ag := &mockSanitizingAgent{}
	in := []byte("keep me\nSECRETSTATE=abc\nkeep me too")

	once := SanitizeTranscriptForStorage(ag, in)
	twice := SanitizeTranscriptForStorage(ag, once)

	if !bytes.Equal(once, twice) {
		t.Errorf("sanitizer is not idempotent:\n once=%q\ntwice=%q", once, twice)
	}
}

func TestSanitizeTranscriptForStorage_NoCapabilityIsPassthrough(t *testing.T) {
	t.Parallel()

	in := []byte("keep me\nSECRETSTATE=abc")
	got := SanitizeTranscriptForStorage(&mockBaseAgent{}, in)

	if !bytes.Equal(got, in) {
		t.Errorf("agent without TranscriptSanitizer should pass through unchanged, got %q", got)
	}
}

func TestSanitizeTranscriptForStorage_NilAgentIsPassthrough(t *testing.T) {
	t.Parallel()

	// Hooks resolve agents best-effort and tolerate a nil Agent, so this must not panic.
	in := []byte("transcript")
	if got := SanitizeTranscriptForStorage(nil, in); !bytes.Equal(got, in) {
		t.Errorf("nil agent should pass through unchanged, got %q", got)
	}
}

func TestSanitizeTranscriptForStorage_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &mockSanitizingAgent{}
	if got := SanitizeTranscriptForStorage(ag, nil); len(got) != 0 {
		t.Errorf("nil input should stay empty, got %q", got)
	}
	if ag.calls != 0 {
		t.Errorf("empty input should not invoke the sanitizer, got %d calls", ag.calls)
	}
}

func TestSanitizeTranscriptForStorage_NilReturnFailsSafe(t *testing.T) {
	t.Parallel()

	// A sanitizer returning nil violates the interface contract; losing the whole
	// transcript is far worse than storing an unsanitized one, so we keep the input.
	ag := &mockSanitizingAgent{returnNil: true}
	in := []byte("keep me")

	if got := SanitizeTranscriptForStorage(ag, in); !bytes.Equal(got, in) {
		t.Errorf("nil sanitizer return should fall back to the input, got %q", got)
	}
}

func TestAsTranscriptSanitizer(t *testing.T) {
	t.Parallel()

	if _, ok := AsTranscriptSanitizer(&mockSanitizingAgent{}); !ok {
		t.Error("agent implementing TranscriptSanitizer should resolve")
	}
	if _, ok := AsTranscriptSanitizer(&mockBaseAgent{}); ok {
		t.Error("agent without TranscriptSanitizer should not resolve")
	}
	if _, ok := AsTranscriptSanitizer(nil); ok {
		t.Error("nil agent should not resolve")
	}
}
