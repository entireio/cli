package transcript

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexItemsRollout is a short current-Codex rollout: a turn whose first call
// is an agent message and a command, closed by a usage snapshot, and whose
// second call is the final message. The response_item line is what the old
// path reads; the item path ignores it.
const codexItemsRollout = `{"timestamp":"2030-01-01T00:00:00.000Z","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"2030-01-01T00:00:01.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"2030-01-01T00:00:01.000Z","type":"turn_context","payload":{"turn_id":"t1","model":"test-model"}}
{"timestamp":"2030-01-01T00:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"list the files"}]}}
{"timestamp":"2030-01-01T00:00:02.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"list the files"}]}}}
{"timestamp":"2030-01-01T00:00:03.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"Listing."}]}}}
{"timestamp":"2030-01-01T00:00:04.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"CommandExecution","id":"e1","command":["/bin/bash","-lc","ls"],"cwd":"file:///work/repo","status":"completed","aggregated_output":"a.txt\n","exit_code":0}}}
{"timestamp":"2030-01-01T00:00:05.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":4}}}}
{"timestamp":"2030-01-01T00:00:06.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m2","content":[{"type":"Text","text":"Done."}]}}}
{"timestamp":"2030-01-01T00:00:07.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":250,"cached_input_tokens":160,"output_tokens":15,"reasoning_output_tokens":4}}}}
{"timestamp":"2030-01-01T00:00:08.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
`

func TestHasCodexItems(t *testing.T) {
	t.Parallel()
	full, err := os.ReadFile(filepath.Join("testdata", "codex_full.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	item := `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"hi"}]}}}` + "\n"
	agent := `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"hi"}]}}}` + "\n"
	started := `{"timestamp":"t","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}` + "\n"
	subagent := `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","item":{"type":"SubAgentActivity","id":"call_1","kind":"started","agent_thread_id":"th2","agent_path":"/root/a"}}}` + "\n"
	old := `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}` + "\n"
	cases := []struct {
		name    string
		content []byte
		want    bool
	}{
		{"current rollout", []byte(codexItemsRollout), true},
		{"older rollout", full, false},
		{"items without task_started", []byte(old + item), false},
		{"first turn without items", []byte(started + old + started + item), false},
		{"first turn with items", []byte(started + old + item + started + old), true},
		{"first turn with only a sub-agent item", []byte(started + old + subagent + started + old + item), false},
		{"first turn with only an agent message item", []byte(started + old + agent + started + old + item), false},
	}
	for _, c := range cases {
		if got := hasCodexItems(c.content); got != c.want {
			t.Errorf("%s: hasCodexItems = %v, want %v", c.name, got, c.want)
		}
	}
}

// Each call's usage is the difference between its closing snapshot and the
// one before, on the call's last assistant line: the first call (m1, e1) gets
// 100-60 = 40 fresh, 60 cached, 10 out, 4 reasoning on e1; the second (m2)
// gets 150-100 = 50 fresh, 100 cached, 5 out, and no reasoning (4-4).
func TestConvertCodexItems(t *testing.T) {
	t.Parallel()
	out, err := Convert([]byte(codexItemsRollout), codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := `{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"2030-01-01T00:00:02.000Z","id":"u1","content":[{"text":"list the files"}]}
{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"2030-01-01T00:00:03.000Z","id":"m1","content":[{"type":"text","text":"Listing."}]}
{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"2030-01-01T00:00:04.000Z","id":"e1","model":"test-model","input_tokens":40,"output_tokens":10,"cache_read_tokens":60,"reasoning_tokens":4,"content":[{"id":"e1","input":{"command":"ls"},"name":"Bash","result":{"output":"a.txt\n","status":"success"},"type":"tool_use"}]}
{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"2030-01-01T00:00:06.000Z","id":"m2","model":"test-model","input_tokens":50,"output_tokens":5,"cache_read_tokens":100,"content":[{"type":"text","text":"Done."}]}
`
	if string(out) != want {
		t.Errorf("Convert:\n got %s\nwant %s", out, want)
	}
}

func TestConvertWithBoundaryCodexItems(t *testing.T) {
	t.Parallel()
	content := []byte(codexItemsRollout)
	full, err := Convert(content, codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	fullLines := strings.SplitAfter(string(full), "\n")
	fullLines = fullLines[:len(fullLines)-1] // the empty string after the last newline
	n := bytes.Count(content, []byte("\n"))
	for start := 1; start <= n; start++ {
		opts := codexItemsOpts
		opts.StartLine = start
		gotFull, boundary, err := ConvertWithBoundary(content, opts)
		if err != nil {
			t.Fatalf("StartLine %d: %v", start, err)
		}
		if !bytes.Equal(gotFull, full) {
			t.Fatalf("StartLine %d: full output differs from Convert", start)
		}
		if boundary > len(fullLines) {
			t.Fatalf("StartLine %d: boundary %d beyond %d lines", start, boundary, len(fullLines))
		}
		delta, err := Convert(content, opts)
		if err != nil {
			t.Fatalf("StartLine %d: delta: %v", start, err)
		}
		if want := strings.Join(fullLines[boundary:], ""); string(delta) != want {
			t.Errorf("StartLine %d: delta is not the full output's last %d lines\ngot:\n%s\nwant:\n%s", start, len(fullLines)-boundary, delta, want)
		}
	}
}

// An item without an ID is skipped; the rest of the rollout converts.
func TestConvertCodexItemsSkipsItemWithoutID(t *testing.T) {
	t.Parallel()
	content := `{"timestamp":"t","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"hi"}]}}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"","content":[{"type":"Text","text":"lost"}]}}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m2","content":[{"type":"Text","text":"Hello"}]}}}
`
	out, err := Convert([]byte(content), codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := `{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"t","id":"u1","content":[{"text":"hi"}]}
{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t","id":"m2","content":[{"type":"text","text":"Hello"}]}
`
	if string(out) != want {
		t.Errorf("Convert:\n got %s\nwant %s", out, want)
	}
}

// An item may carry the words the converter scans for (here as an MCP
// argument key): it is still an item, not a turn_context line.
func TestConvertCodexItemsWordsInItems(t *testing.T) {
	t.Parallel()
	content := `{"timestamp":"t","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"hi"}]}}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"Hello"}]}}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"McpToolCall","id":"c1","server":"s","tool":"t","status":"completed","arguments":{"turn_context":1}}}}
`
	out, err := Convert([]byte(content), codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(string(out), `"id":"c1"`) {
		t.Errorf("the MCP call naming turn_context was dropped:\n%s", out)
	}
}

// A call with no assistant line of its own (reasoning only) gets a usage-only
// line; a repeated snapshot adds nothing; a user message ends the open call.
func TestConvertCodexItemsCallUsage(t *testing.T) {
	t.Parallel()
	tc := func(ts string, in, cached, out int) string {
		return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`, ts, in, cached, out) + "\n"
	}
	content := `{"timestamp":"t0","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t0","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"t0","type":"turn_context","payload":{"turn_id":"t1","model":"m1"}}
{"timestamp":"t1","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"hi"}]}}}
{"timestamp":"t2","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"Reasoning","id":"r1"}}}
` + tc("t3", 10, 4, 2) + tc("t4", 10, 4, 2) + `{"timestamp":"t5","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"Hello"}]}}}
` + tc("t6", 25, 10, 5)
	out, err := Convert([]byte(content), codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := `{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"t1","id":"u1","content":[{"text":"hi"}]}
{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t3","model":"m1","input_tokens":6,"output_tokens":2,"cache_read_tokens":4,"content":null}
{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t5","id":"m1","model":"m1","input_tokens":9,"output_tokens":3,"cache_read_tokens":6,"content":[{"type":"text","text":"Hello"}]}
`
	if string(out) != want {
		t.Errorf("Convert:\n got %s\nwant %s", out, want)
	}
}

// A prompt Codex records only as a response_item, with the turn's agent
// message as an item, keeps the response_item path, so the prompt is kept.
func TestConvertCodexPromptOnlyAsResponseItem(t *testing.T) {
	t.Parallel()
	content := `{"timestamp":"t0","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t1","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"FIRST PROMPT"}]}}
{"timestamp":"t3","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"Hello"}]}}}
{"timestamp":"t4","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
`
	if hasCodexItems([]byte(content)) {
		t.Fatal("hasCodexItems = true, want false")
	}
	out, err := Convert([]byte(content), codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(string(out), `"type":"user","ts":"t2","content":[{"text":"FIRST PROMPT"}]`) {
		t.Errorf("the prompt was dropped:\n%s", out)
	}
}

// A thread_spawn sub-agent's rollout has no items in its first turn and gets
// its task prompt as a response_item: it keeps the response_item path and
// its user lines.
func TestConvertCodexSubAgentRollout(t *testing.T) {
	t.Parallel()
	content := `{"timestamp":"t0","type":"session_meta","payload":{"id":"s2","source":{"subagent":{"thread_spawn":{"parent_thread_id":"s1","depth":1}}}}}
{"timestamp":"t1","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Ready."}]}}
{"timestamp":"t3","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
{"timestamp":"t4","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}
{"timestamp":"t5","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"check the tests"}]}}
{"timestamp":"t6","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"check the tests"}]}}}
{"timestamp":"t7","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"They pass."}]}}}
{"timestamp":"t8","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"They pass."}]}}
`
	if hasCodexItems([]byte(content)) {
		t.Fatal("hasCodexItems = true, want false")
	}
	out, err := Convert([]byte(content), codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if n := strings.Count(string(out), `"type":"user"`); n != 1 || !strings.Contains(string(out), `"text":"check the tests"`) {
		t.Errorf("want the one task prompt as a user line, got %d user lines:\n%s", n, out)
	}
}
