package agentimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFactoryDiscover_LookbackAndFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	writeAged := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	writeAged("recent.jsonl", 5*24*time.Hour)
	writeAged("old.jsonl", 60*24*time.Hour)

	got, err := factoryImporter{}.Discover("", dir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != fxRecent {
		t.Fatalf("lookback filter wrong: %v", got)
	}

	got, err = factoryImporter{}.Discover("", dir, now, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("old session is outside lookback even when filtered: %v", got)
	}
}

func TestFactorySplitTurns_PromptsTokensSettingsModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "sess.jsonl")
	// Droid envelope: {"type":"message","id":..,"message":{"role":..,"content":..}}.
	full := []byte(strings.Join([]string{
		`{"type":"session_start","id":"s0"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"first"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","id":"m1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"message","id":"u2","message":{"role":"user","content":"second"}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","id":"m2","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":20,"output_tokens":7}}}`,
	}, "\n") + "\n")
	if err := os.WriteFile(sessPath, full, 0o644); err != nil {
		t.Fatal(err)
	}
	// Droid carries no per-message timestamp, so turns fall back to the file
	// modtime; pin it so we can assert CreatedAt is populated from it.
	modTime := time.Date(2026, 6, 24, 9, 30, 0, 0, time.UTC)
	if err := os.Chtimes(sessPath, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	// Model comes from the adjacent <session>.settings.json, not the transcript.
	if err := os.WriteFile(filepath.Join(dir, "sess.settings.json"), []byte(`{"model":"custom:Gemini-2.5-Pro-0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	turns, err := factoryImporter{}.SplitTurns(SessionFile{Path: sessPath, SessionID: "sess"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[0].LineStart != 1 || turns[0].LineEnd != 3 {
		t.Errorf("turn0 bounds = [%d,%d), want [1,3)", turns[0].LineStart, turns[0].LineEnd)
	}
	if turns[0].Prompt != fxFirst || turns[1].Prompt != fxSecond {
		t.Errorf("prompts = %q,%q", turns[0].Prompt, turns[1].Prompt)
	}
	if turns[0].UUID != "u1" {
		t.Errorf("turn0 uuid = %q, want u1", turns[0].UUID)
	}
	if turns[0].Model != "Gemini-2.5-Pro-0" {
		t.Errorf("turn0 model = %q, want Gemini-2.5-Pro-0 (cleaned from settings)", turns[0].Model)
	}
	if !turns[0].CreatedAt.Equal(modTime) {
		t.Errorf("turn0 CreatedAt = %v, want file modtime %v", turns[0].CreatedAt, modTime)
	}
	if turns[0].Tokens == nil || turns[0].Tokens.OutputTokens != 5 {
		t.Errorf("turn0 tokens not bounded to its own turn: %+v", turns[0].Tokens)
	}
	if turns[1].Tokens == nil || turns[1].Tokens.OutputTokens != 7 {
		t.Errorf("turn1 tokens wrong: %+v", turns[1].Tokens)
	}
}

func TestFactorySplitTurns_ToolResultIsNotATurn(t *testing.T) {
	t.Parallel()
	full := []byte(strings.Join([]string{
		`{"type":"message","id":"u1","message":{"role":"user","content":"do it"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}}`,
	}, "\n") + "\n")
	turns, err := factoryImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "s.jsonl"), SessionID: "s"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("tool_result must not start a turn; want 1 turn, got %d", len(turns))
	}
}
