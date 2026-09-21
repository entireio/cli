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

func TestCodexSplitTurns_ExtractsPairedGitCommitSHAs(t *testing.T) {
	t.Parallel()
	full := []byte(codexSession("s1", "/work/myrepo",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"commit it"}]}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call-1","input":"git status && git commit -m 'first'"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"Process exited with code 0.\nFinal output:"},{"type":"input_text","text":"[topic fe71aa6] first"},{"type":"input_text","text":"exit_code=0"}]}}`,
		`not valid json {{{`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call-2","input":"git -C /work/myrepo commit --amend --no-edit"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-2","output":"[topic aabbccd] first"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}}`,
	))

	turns, err := codexImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "r.jsonl"), SessionID: "s1"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	want := []string{"fe71aa6", "aabbccd"}
	if strings.Join(turns[0].CommitSHAs, ",") != strings.Join(want, ",") {
		t.Fatalf("turn0 CommitSHAs = %v, want %v", turns[0].CommitSHAs, want)
	}
	if len(turns[1].CommitSHAs) != 0 {
		t.Fatalf("turn1 CommitSHAs = %v, want empty", turns[1].CommitSHAs)
	}
}

func TestCodexCommitSHAsInRange_RejectsUnpairedFailedAndUnrelatedHex(t *testing.T) {
	t.Parallel()
	raw := splitRawLines([]byte(strings.Join([]string{
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"status","input":"git status"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"status","output":"deadbeef"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"failed","input":"git commit -m failed"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"failed","output":{"exit_code":1,"output":"[topic badc0de] failed"}}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"unpaired","input":"git commit -m unpaired"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"other","output":"[topic cafebabe] wrong call"}}`,
	}, "\n")))
	if got := codexCommitSHAsInRange(raw, 0, len(raw)); len(got) != 0 {
		t.Fatalf("CommitSHAs = %v, want empty", got)
	}
}

func TestCodexCommitSHAsInRange_CommandBoundaries(t *testing.T) {
	t.Parallel()
	raw := splitRawLines([]byte(strings.Join([]string{
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"quoted-c","input":"git -C '/work/my repo' commit -m ok"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"quoted-c","output":[{"type":"input_text","text":"[topic 1234567] ok"}]}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"word","input":"echo git commit"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"word","output":"[topic 7654321] echoed"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"quoted","input":"printf 'x; git commit -m wrong'"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"quoted","output":"[topic abcdef0] quoted"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"escaped","input":"printf x\\; git commit -m wrong"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"escaped","output":"[topic fedcba0] escaped"}}`,
	}, "\n")))
	want := []string{"1234567"}
	if got := codexCommitSHAsInRange(raw, 0, len(raw)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("CommitSHAs = %v, want %v", got, want)
	}
}

func TestCodexSplitTurns_ExecEnvelopeVariants(t *testing.T) {
	t.Parallel()
	full := []byte(codexSession("s1", "/work/myrepo",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"commit both"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"fn","arguments":"{\"cmd\":\"git commit -m function\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"fn","output":"Process exited with code 0.\n[topic 1111111] function"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"js","input":"const r = await tools.exec_command({cmd:\"git status && git commit -m wrapped\",workdir:\"/work/myrepo\"}); text(r.output);"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"js","output":[{"type":"input_text","text":"[topic 2222222] wrapped"}]}}`,
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"call_id\":\"dynamic\",\"input\":\"await tools.exec_command({cmd:`git commit -m ${message}`})\"}}",
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"dynamic","output":"[topic 3333333] dynamic"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"other-object","input":"await tools.exec_command({workdir:\"/work/myrepo\"}); const unrelated = {cmd:\"git commit -m wrong\"}"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"other-object","output":"[topic 4444444] wrong"}}`,
	))
	turns, err := codexImporter{}.SplitTurns(SessionFile{Path: filepath.Join(t.TempDir(), "r.jsonl"), SessionID: "s1"}, full)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1111111", "2222222"}
	if len(turns) != 1 || strings.Join(turns[0].CommitSHAs, ",") != strings.Join(want, ",") {
		t.Fatalf("CommitSHAs = %v, want %v", turns[0].CommitSHAs, want)
	}
}
