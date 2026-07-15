package agentimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexSession builds a Codex rollout transcript whose session_meta carries the
// given id and cwd.
func codexSession(id, cwd string, body ...string) string {
	meta := `{"timestamp":"2026-06-20T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `"}}`
	return strings.Join(append([]string{meta}, body...), "\n") + "\n"
}

func TestCodexDiscover_RepoFilterLookbackRecursive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	repoRoot := "/work/myrepo"
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)

	writeRollout := func(rel, id, cwd string, age time.Duration) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(codexSession(id, cwd)), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// In repo, recent (date-sharded path → exercises recursive walk).
	writeRollout("2026/06/20/rollout-a-mine.jsonl", "mine", repoRoot, 5*24*time.Hour)
	// In a subdir of the repo → still matches.
	writeRollout("2026/06/20/rollout-b-sub.jsonl", "sub", repoRoot+"/pkg", 5*24*time.Hour)
	// Different repo → excluded.
	writeRollout("2026/06/20/rollout-c-other.jsonl", "other", "/work/elsewhere", 5*24*time.Hour)
	// In repo but outside lookback → excluded.
	writeRollout("2026/05/01/rollout-d-old.jsonl", "old", repoRoot, 60*24*time.Hour)

	got, err := codexImporter{}.Discover(repoRoot, dir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := map[string]bool{}
	for _, sf := range got {
		gotIDs[sf.SessionID] = true
	}
	if len(got) != 2 || !gotIDs["mine"] || !gotIDs["sub"] {
		t.Fatalf("repo/lookback filter wrong, got %v", gotIDs)
	}

	got, err = codexImporter{}.Discover(repoRoot, dir, now, []string{"mine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "mine" {
		t.Fatalf("session filter wrong: %v", got)
	}
}

func TestCodexSplitTurns_PromptsAndTokenDelta(t *testing.T) {
	t.Parallel()
	full := []byte(codexSession("s1", "/work/myrepo",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5,"total_tokens":15}}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"cached_input_tokens":0,"output_tokens":12,"total_tokens":42}}}}`,
	))

	turns, err := codexImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "r.jsonl"), SessionID: "s1"}, full)
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
	// Codex reports cumulative usage; the per-turn delta must be scoped.
	if turns[0].Tokens == nil || turns[0].Tokens.OutputTokens != 5 {
		t.Errorf("turn0 token delta wrong: %+v", turns[0].Tokens)
	}
	if turns[1].Tokens == nil || turns[1].Tokens.OutputTokens != 7 {
		t.Errorf("turn1 token delta = %+v, want output 7 (12-5)", turns[1].Tokens)
	}
}

func TestCodexSplitTurns_NonUserResponseItemIsNotATurn(t *testing.T) {
	t.Parallel()
	full := []byte(codexSession("s1", "/work/myrepo",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","input":"ls"}}`,
	))
	turns, err := codexImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "r.jsonl"), SessionID: "s1"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("only the user message starts a turn; want 1, got %d", len(turns))
	}
}
