package agentimport

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGeminiDiscover_LookbackAndFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	writeAged := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`{"messages":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	writeAged("session-2026-06-20-recent01.json", 5*24*time.Hour)
	writeAged("session-2026-04-01-old00001.json", 60*24*time.Hour)
	writeAged("notes.txt", 1*time.Hour)

	got, err := geminiImporter{}.Discover("", dir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "session-2026-06-20-recent01" {
		t.Fatalf("lookback/extension filter wrong: %v", got)
	}

	got, err = geminiImporter{}.Discover("", dir, now, []string{"session-2026-06-20-recent01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("session filter wrong: %v", got)
	}
}

// TestGeminiSplitTurns_OneCheckpointPerSession verifies Gemini imports at
// session granularity: a single Turn covering the whole (message-indexed)
// transcript, with whole-session tokens and the first user prompt.
func TestGeminiSplitTurns_OneCheckpointPerSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "session-x.json")
	full := []byte(`{"messages":[` +
		`{"type":"user","id":"u1","content":[{"text":"first"}]},` +
		`{"type":"gemini","id":"g1","content":"ok","tokens":{"input":10,"output":5}},` +
		`{"type":"user","id":"u2","content":[{"text":"second"}]},` +
		`{"type":"gemini","id":"g2","content":"done","tokens":{"input":20,"output":7}}` +
		`]}`)
	if err := os.WriteFile(p, full, 0o644); err != nil {
		t.Fatal(err)
	}

	turns, err := geminiImporter{}.SplitTurns(SessionFile{Path: p, SessionID: "session-x"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("gemini imports per-session; want 1 turn, got %d", len(turns))
	}
	turn := turns[0]
	if turn.LineStart != 0 || turn.LineEnd != 4 {
		t.Errorf("turn bounds = [%d,%d), want [0,4) in message-index space", turn.LineStart, turn.LineEnd)
	}
	if turn.UUID != "session-x" {
		t.Errorf("per-session turn uuid = %q, want session-x (stable/idempotent)", turn.UUID)
	}
	if turn.Prompt != "first" {
		t.Errorf("prompt = %q, want the first user prompt", turn.Prompt)
	}
	// Whole-session totals: 5 + 7 output, 10 + 20 input.
	if turn.Tokens == nil || turn.Tokens.OutputTokens != 12 || turn.Tokens.InputTokens != 30 {
		t.Errorf("session tokens wrong: %+v", turn.Tokens)
	}
}

func TestGeminiSplitTurns_EmptyTranscriptNoTurns(t *testing.T) {
	t.Parallel()
	turns, err := geminiImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "s.json"), SessionID: "s"}, []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("empty transcript should yield no turns, got %d", len(turns))
	}
}
