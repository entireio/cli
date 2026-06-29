package agentimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCursorDiscover_FlatLookbackAndFilter(t *testing.T) {
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
	writeAged("skip.txt", 1*time.Hour)

	imp := cursorImporter{}
	got, err := imp.Discover("", dir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != fxRecent {
		t.Fatalf("lookback filter wrong: %v", got)
	}

	writeAged("abc123.jsonl", 1*24*time.Hour)
	got, err = imp.Discover("", dir, now, []string{"abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "abc123" {
		t.Fatalf("session filter wrong: %v", got)
	}
}

// TestCursorDiscover_NestedLayout covers Cursor's IDE layout where the transcript
// lives at <dir>/<id>/<id>.jsonl rather than flat at <dir>/<id>.jsonl.
func TestCursorDiscover_NestedLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	sessID := "nested-sess"
	nestedDir := filepath.Join(dir, sessID)
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(nestedDir, sessID+".jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := now.Add(-2 * 24 * time.Hour)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}

	got, err := cursorImporter{}.Discover("", dir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != sessID {
		t.Fatalf("nested discover wrong: %v", got)
	}
	if got[0].Path != p {
		t.Fatalf("nested path = %q, want %q", got[0].Path, p)
	}
}

func TestCursorDiscover_MissingDirIsEmpty(t *testing.T) {
	t.Parallel()
	got, err := cursorImporter{}.Discover("", filepath.Join(t.TempDir(), "nope"), time.Now(), nil)
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestCursorSplitTurns_TwoPromptsNoTokensNoModel(t *testing.T) {
	t.Parallel()
	// Real Cursor lines use "role" (not "type"), carry no per-turn uuid or
	// timestamp, and record neither model nor token usage (see cursor/AGENT.md).
	full := []byte(strings.Join([]string{
		`{"role":"user","message":{"role":"user","content":"first"}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"role":"user","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n")
	// Write the transcript so the importer's modtime CreatedAt fallback has a
	// real file to stat.
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, full, 0o644); err != nil {
		t.Fatal(err)
	}

	turns, err := cursorImporter{}.SplitTurns(SessionFile{Path: p, SessionID: "s"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[0].LineStart != 0 || turns[0].LineEnd != 2 {
		t.Errorf("turn0 bounds = [%d,%d), want [0,2)", turns[0].LineStart, turns[0].LineEnd)
	}
	if turns[0].Prompt != fxFirst || turns[1].Prompt != fxSecond {
		t.Errorf("prompts = %q,%q", turns[0].Prompt, turns[1].Prompt)
	}
	// Each turn must get a distinct (line-index) key; an empty/duplicate UUID
	// would collide on one checkpoint ID and drop every turn after the first.
	if turns[0].UUID == "" || turns[1].UUID == "" || turns[0].UUID == turns[1].UUID {
		t.Errorf("turn UUIDs must be non-empty and distinct, got %q and %q", turns[0].UUID, turns[1].UUID)
	}
	if turns[0].CreatedAt.IsZero() {
		t.Errorf("CreatedAt should fall back to the file modtime, got zero")
	}
	if turns[0].Tokens != nil {
		t.Errorf("cursor records no tokens, want nil, got %+v", turns[0].Tokens)
	}
	if turns[0].Model != "" {
		t.Errorf("cursor records no model, want empty, got %q", turns[0].Model)
	}
}

func TestCursorSplitTurns_ToolResultIsNotATurn(t *testing.T) {
	t.Parallel()
	full := []byte(strings.Join([]string{
		`{"role":"user","message":{"role":"user","content":"do it"}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}}`,
	}, "\n") + "\n")
	turns, err := cursorImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "s.jsonl"), SessionID: "s"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("tool_result must not start a turn; want 1 turn, got %d", len(turns))
	}
}
