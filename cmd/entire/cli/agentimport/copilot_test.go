package agentimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copilotSession writes <base>/<id>/events.jsonl with a session.start carrying
// gitRoot, and returns nothing (sets modtime via age).
func writeCopilotSession(t *testing.T, base, id, gitRoot string, age time.Duration, now time.Time, body ...string) {
	t.Helper()
	sdir := filepath.Join(base, id)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	start := `{"type":"session.start","id":"s0","timestamp":"2026-06-20T00:00:00Z","data":{"context":{"cwd":"` + gitRoot + `","gitRoot":"` + gitRoot + `"}}}`
	content := strings.Join(append([]string{start}, body...), "\n") + "\n"
	p := filepath.Join(sdir, "events.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := now.Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestCopilotDiscover_RepoFilterAndLookback(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repoRoot := "/work/myrepo"
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)

	writeCopilotSession(t, base, "mine", repoRoot, 5*24*time.Hour, now)
	writeCopilotSession(t, base, "other", "/work/elsewhere", 5*24*time.Hour, now)
	writeCopilotSession(t, base, "old", repoRoot, 60*24*time.Hour, now)

	got, err := copilotImporter{}.Discover(repoRoot, base, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "mine" {
		t.Fatalf("repo/lookback filter wrong: %v", got)
	}

	got, err = copilotImporter{}.Discover(repoRoot, base, now, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("old session is outside lookback even when filtered: %v", got)
	}
}

func TestCopilotSplitTurns_PromptsTokensModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	full := []byte(strings.Join([]string{
		`{"type":"session.start","id":"s0","timestamp":"2026-06-20T00:00:00Z","data":{"context":{"gitRoot":"/work/myrepo"}}}`,
		`{"type":"session.model_change","id":"mc","data":{"newModel":"gpt-5"}}`,
		`{"type":"user.message","id":"u1","timestamp":"2026-06-20T00:00:01Z","data":{"content":"first"}}`,
		`{"type":"assistant.message","id":"a1","data":{"content":"ok","outputTokens":5}}`,
		`{"type":"user.message","id":"u2","timestamp":"2026-06-20T00:01:00Z","data":{"content":"second"}}`,
		`{"type":"assistant.message","id":"a2","data":{"content":"done","outputTokens":7}}`,
	}, "\n") + "\n")
	if err := os.WriteFile(p, full, 0o644); err != nil {
		t.Fatal(err)
	}

	turns, err := copilotImporter{}.SplitTurns(SessionFile{Path: p, SessionID: "sess"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[0].LineStart != 2 || turns[0].LineEnd != 4 {
		t.Errorf("turn0 bounds = [%d,%d), want [2,4)", turns[0].LineStart, turns[0].LineEnd)
	}
	if turns[0].Prompt != fxFirst || turns[1].Prompt != fxSecond {
		t.Errorf("prompts = %q,%q", turns[0].Prompt, turns[1].Prompt)
	}
	if turns[0].UUID != "u1" {
		t.Errorf("turn0 uuid = %q, want u1", turns[0].UUID)
	}
	if turns[0].Model != "gpt-5" {
		t.Errorf("turn0 model = %q, want gpt-5", turns[0].Model)
	}
	if turns[0].Tokens == nil || turns[0].Tokens.OutputTokens != 5 {
		t.Errorf("turn0 tokens not bounded to its own turn: %+v", turns[0].Tokens)
	}
	if turns[1].Tokens == nil || turns[1].Tokens.OutputTokens != 7 {
		t.Errorf("turn1 tokens wrong: %+v", turns[1].Tokens)
	}
}

// TestCopilotSplitTurns_NumericTimestamp covers the dual-format timestamp:
// Copilot may emit a numeric epoch-millis timestamp instead of an RFC3339
// string. Decoding it as a plain string would fail json.Unmarshal and silently
// drop the turn, importing zero turns for the session.
func TestCopilotSplitTurns_NumericTimestamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	const epochMillis = 1750377601000 // 2025-06-20T00:00:01Z
	full := []byte(strings.Join([]string{
		`{"type":"session.start","id":"s0","timestamp":1750377600000,"data":{"context":{"gitRoot":"/work/myrepo"}}}`,
		`{"type":"user.message","id":"u1","timestamp":1750377601000,"data":{"content":"first"}}`,
		`{"type":"assistant.message","id":"a1","data":{"content":"ok","outputTokens":5}}`,
	}, "\n") + "\n")
	if err := os.WriteFile(p, full, 0o644); err != nil {
		t.Fatal(err)
	}

	turns, err := copilotImporter{}.SplitTurns(SessionFile{Path: p, SessionID: "sess"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("numeric timestamp must not drop the turn; want 1, got %d", len(turns))
	}
	if want := time.UnixMilli(epochMillis); !turns[0].CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v (decoded from epoch-millis)", turns[0].CreatedAt, want)
	}
}

func TestCopilotSplitTurns_NonUserEventIsNotATurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	full := []byte(strings.Join([]string{
		`{"type":"user.message","id":"u1","data":{"content":"do it"}}`,
		`{"type":"tool.execution_complete","id":"t1","data":{"toolCallId":"x"}}`,
		`{"type":"assistant.message","id":"a1","data":{"content":"done","outputTokens":3}}`,
	}, "\n") + "\n")
	if werr := os.WriteFile(p, full, 0o644); werr != nil {
		t.Fatal(werr)
	}
	turns, err := copilotImporter{}.SplitTurns(SessionFile{Path: p, SessionID: "sess"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("only user.message starts a turn; want 1, got %d", len(turns))
	}
}
