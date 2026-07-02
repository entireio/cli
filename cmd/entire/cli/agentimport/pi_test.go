package agentimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiDiscover_LookbackFilterAndSessionID(t *testing.T) {
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
	// Pi names files <timestamp>_<uuid>.jsonl; the session ID is the uuid suffix.
	writeAged("2026-06-20T00-00-00-000Z_sessA.jsonl", 5*24*time.Hour)
	writeAged("2026-04-01T00-00-00-000Z_old.jsonl", 60*24*time.Hour)

	got, err := piImporter{}.Discover("", dir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "sessA" {
		t.Fatalf("lookback/session-id wrong: %v", got)
	}

	got, err = piImporter{}.Discover("", dir, now, []string{"sessA"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "sessA" {
		t.Fatalf("session filter wrong: %v", got)
	}
}

func TestPiSplitTurns_PromptsTokensModel(t *testing.T) {
	t.Parallel()
	full := []byte(strings.Join([]string{
		`{"type":"session","id":"s0","timestamp":"2026-06-20T00:00:00Z"}`,
		`{"type":"message","id":"u1","timestamp":"2026-06-20T00:00:01Z","message":{"role":"user","content":"first"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-06-20T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"model":"gpt-5.5","usage":{"input":10,"output":5}}}`,
		`{"type":"message","id":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
		`{"type":"message","id":"a2","timestamp":"2026-06-20T00:01:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"model":"gpt-5.5","usage":{"input":20,"output":7}}}`,
	}, "\n") + "\n")

	turns, err := piImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "s.jsonl"), SessionID: "s"}, full)
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
	if turns[0].Model != "gpt-5.5" {
		t.Errorf("turn0 model = %q, want gpt-5.5", turns[0].Model)
	}
	if turns[0].Tokens == nil || turns[0].Tokens.OutputTokens != 5 {
		t.Errorf("turn0 tokens not bounded to its own turn: %+v", turns[0].Tokens)
	}
	if turns[1].Tokens == nil || turns[1].Tokens.OutputTokens != 7 {
		t.Errorf("turn1 tokens wrong: %+v", turns[1].Tokens)
	}
}

// TestPiSplitTurns_ModelInheritedOverPrefix guards the branch-resolution fix:
// a later turn whose own assistant message omits the model must still resolve
// the model from an earlier active-branch message. This only works when the
// model is extracted over the [0,end) prefix (chains intact); extracting over
// the [start,end) slice would strip the earlier model and yield "".
func TestPiSplitTurns_ModelInheritedOverPrefix(t *testing.T) {
	t.Parallel()
	full := []byte(strings.Join([]string{
		`{"type":"message","id":"pu1","message":{"role":"user","content":"first"}}`,
		`{"type":"message","id":"pa1","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"model":"model-A","usage":{"input":10,"output":5}}}`,
		`{"type":"message","id":"pu2","message":{"role":"user","content":"second"}}`,
		`{"type":"message","id":"pa2","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input":20,"output":7}}}`,
	}, "\n") + "\n")

	turns, err := piImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "s.jsonl"), SessionID: "s"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[1].Model != "model-A" {
		t.Errorf("turn1 model = %q, want model-A inherited from the active branch", turns[1].Model)
	}
}

func TestPiSplitTurns_ToolResultIsNotATurn(t *testing.T) {
	t.Parallel()
	// A toolResult-role message is not a user prompt and must not start a turn.
	full := []byte(strings.Join([]string{
		`{"type":"message","id":"u1","message":{"role":"user","content":"do it"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","name":"edit","id":"t1","arguments":{}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","content":"out","toolCallId":"t1"}}`,
	}, "\n") + "\n")
	turns, err := piImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "s.jsonl"), SessionID: "s"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("non-user message must not start a turn; want 1, got %d", len(turns))
	}
}
