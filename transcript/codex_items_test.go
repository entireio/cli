package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/transcript/codexitem"
)

// 2030-01-01T00:00:00.352Z and a second later.
const (
	codexItemStartMs = int64(1893456000352)
	codexItemDoneMs  = codexItemStartMs + 1000
	codexItemStartTS = `"2030-01-01T00:00:00.352Z"`
	codexItemDoneTS  = `"2030-01-01T00:00:01.352Z"`
)

var codexItemsOpts = Options{Agent: "codex", CLIVersion: "0.5.1"}

func codexIntp(n int) *int { return &n }

// codexRecordLines encodes records one JSON object per line, for comparison.
func codexRecordLines(t *testing.T, records []Record) string {
	t.Helper()
	var lines []string
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		lines = append(lines, string(b))
	}
	return strings.Join(lines, "\n")
}

func assertCodexRecords(t *testing.T, it codexitem.Item, want ...string) {
	t.Helper()
	got := codexRecordLines(t, codexItemRecords(it))
	if w := strings.Join(want, "\n"); got != w {
		t.Errorf("records differ\ngot:\n%s\nwant:\n%s", got, w)
	}
}

// doneCodexItem is a completed item with both times set.
func doneCodexItem(it codexitem.Item) codexitem.Item {
	if it.Status == "" {
		it.Status = codexitem.StatusCompleted
	}
	it.TurnID = "t1"
	it.StartedAtMs, it.CompletedAtMs = codexItemStartMs, codexItemDoneMs
	return it
}

func TestCodexItemRecordsUser(t *testing.T) {
	t.Parallel()
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "u1", Kind: codexitem.KindUser, Parts: []string{
		"<environment_context>\n  <cwd>/w</cwd>\n</environment_context>",
		"fix the bug",
		"",
		"and the test",
	}}),
		`{"op":"message","id":"u1","sourceId":"u1","role":"user","ts":`+codexItemDoneTS+`,"content":[{"text":"fix the bug\n\nand the test"}]}`)

	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "u2", Kind: codexitem.KindUser, Parts: []string{
		"<environment_context>\n  <cwd>/w</cwd>\n</environment_context>",
	}}), "")
}

func TestCodexItemRecordsAgent(t *testing.T) {
	t.Parallel()
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "m1", Kind: codexitem.KindAgent, Text: "Done."}),
		`{"op":"message","id":"m1","sourceId":"m1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"text","text":"Done."}]}`)
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "m2", Kind: codexitem.KindAgent}))
}

func TestCodexItemRecordsCommand(t *testing.T) {
	t.Parallel()
	use := `{"op":"message","id":"e1","sourceId":"e1","role":"assistant","ts":%s,"content":[{"type":"tool_use","id":"e1","name":"Bash","input":{"command":"ls -la"}}]}`
	result := `{"op":"result","id":"e1","sourceId":"e1","ts":%s,"toolUseId":"e1","result":{"output":%q,"status":%q}}`
	cmd := codexitem.Item{ID: "e1", Kind: codexitem.KindCommand, Command: "ls -la", CWD: "/w"}

	ok := cmd
	ok.Output, ok.ExitCode = "total 0\n", codexIntp(0)
	assertCodexRecords(t, doneCodexItem(ok),
		fmt.Sprintf(use, codexItemDoneTS), fmt.Sprintf(result, codexItemDoneTS, "total 0\n", "success"))

	failed := cmd
	failed.Output, failed.ExitCode = "ls: x: No such file\n", codexIntp(2)
	assertCodexRecords(t, doneCodexItem(failed),
		fmt.Sprintf(use, codexItemDoneTS), fmt.Sprintf(result, codexItemDoneTS, "ls: x: No such file\n", "error"))

	running := cmd
	running.Status, running.StartedAtMs = codexitem.StatusInProgress, codexItemStartMs
	assertCodexRecords(t, running, fmt.Sprintf(use, codexItemStartTS))
	running.Output = "total"
	assertCodexRecords(t, running,
		fmt.Sprintf(use, codexItemStartTS), fmt.Sprintf(result, codexItemStartTS, "total", "success"))
}

func TestCodexItemRecordsFileChange(t *testing.T) {
	t.Parallel()
	it := doneCodexItem(codexitem.Item{ID: "f1", Kind: codexitem.KindFileChange, Changes: []codexitem.Change{
		{Path: "/w/a.txt", Kind: "add", Diff: "hi\n"},
		{Path: "/w/b.go", Kind: "update", MovePath: "/w/c.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
	}})
	assertCodexRecords(t, it,
		`{"op":"message","id":"f1","sourceId":"f1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"f1","name":"FileChange","input":{"changes":[{"path":"/w/a.txt","kind":"add","diff":"hi\n"},{"path":"/w/b.go","kind":"update","move_path":"/w/c.go","diff":"@@ -1 +1 @@\n-a\n+b\n"}]}}]}`,
		`{"op":"result","id":"f1","sourceId":"f1","ts":`+codexItemDoneTS+`,"toolUseId":"f1","result":{"output":"","status":"success"}}`)

	it.Status = codexitem.StatusDeclined
	recs := codexItemRecords(it)
	if len(recs) != 2 || recs[1].Result == nil || recs[1].Result.Status != ToolStatusError {
		t.Errorf("declined file change: want an error result, got %s", codexRecordLines(t, recs))
	}
}

func TestCodexItemRecordsMCPTool(t *testing.T) {
	t.Parallel()
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "c1", Kind: codexitem.KindMCPTool, Server: "test_server", Tool: "echo",
		Arguments: json.RawMessage(`{"text":"hi"}`), Output: "hi"}),
		`{"op":"message","id":"c1","sourceId":"c1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"c1","name":"test_server.echo","input":{"text":"hi"}}]}`,
		`{"op":"result","id":"c1","sourceId":"c1","ts":`+codexItemDoneTS+`,"toolUseId":"c1","result":{"output":"hi","status":"success"}}`)

	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "c2", Kind: codexitem.KindMCPTool, Server: "s", Tool: "t",
		Status: codexitem.StatusFailed, Error: "denied"}),
		`{"op":"message","id":"c2","sourceId":"c2","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"c2","name":"s.t","input":{}}]}`,
		`{"op":"result","id":"c2","sourceId":"c2","ts":`+codexItemDoneTS+`,"toolUseId":"c2","result":{"output":"denied","status":"error"}}`)

	// Output and an error flag: the output is kept, the status is error.
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "c4", Kind: codexitem.KindMCPTool, Server: "s", Tool: "t",
		Output: "boom", Error: "tool reported an error"}),
		`{"op":"message","id":"c4","sourceId":"c4","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"c4","name":"s.t","input":{}}]}`,
		`{"op":"result","id":"c4","sourceId":"c4","ts":`+codexItemDoneTS+`,"toolUseId":"c4","result":{"output":"boom","status":"error"}}`)

	// Running: the call, no result yet.
	assertCodexRecords(t, codexitem.Item{ID: "c3", Kind: codexitem.KindMCPTool, Server: "s", Tool: "t", Status: codexitem.StatusInProgress},
		`{"op":"message","id":"c3","sourceId":"c3","role":"assistant","content":[{"type":"tool_use","id":"c3","name":"s.t","input":{}}]}`)
}

func TestCodexItemRecordsWebSearchAndImageView(t *testing.T) {
	t.Parallel()
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "w1", Kind: codexitem.KindWebSearch, Query: "example query"}),
		`{"op":"message","id":"w1","sourceId":"w1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"w1","name":"WebSearch","input":{"query":"example query"}}]}`,
		`{"op":"result","id":"w1","sourceId":"w1","ts":`+codexItemDoneTS+`,"toolUseId":"w1","result":{"output":"","status":"success"}}`)
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "i1", Kind: codexitem.KindImageView, Path: "/w/shot.png"}),
		`{"op":"message","id":"i1","sourceId":"i1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"i1","name":"ImageView","input":{"path":"/w/shot.png"}}]}`,
		`{"op":"result","id":"i1","sourceId":"i1","ts":`+codexItemDoneTS+`,"toolUseId":"i1","result":{"output":"","status":"success"}}`)
}

func TestCodexItemRecordsSpawns(t *testing.T) {
	t.Parallel()
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "k1", Kind: codexitem.KindCollabTool, Tool: "spawnAgent",
		Prompt: "look into it", ReceiverThreadIDs: []string{"th2"}}),
		`{"op":"message","id":"k1","sourceId":"k1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"k1","name":"spawnAgent","input":{"prompt":"look into it"}}]}`,
		`{"op":"result","id":"k1","sourceId":"k1","ts":`+codexItemDoneTS+`,"toolUseId":"k1","result":{"output":"","status":"success","agent":{"id":"th2"}}}`)

	// Another collab tool: no agent, and no prompt in the input.
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "k2", Kind: codexitem.KindCollabTool, Tool: "wait", ReceiverThreadIDs: []string{"th2"}}),
		`{"op":"message","id":"k2","sourceId":"k2","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"k2","name":"wait","input":{}}]}`,
		`{"op":"result","id":"k2","sourceId":"k2","ts":`+codexItemDoneTS+`,"toolUseId":"k2","result":{"output":"","status":"success"}}`)

	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "s1", Kind: codexitem.KindSubAgent, ActivityKind: "started",
		AgentThreadID: "th3", AgentPath: "/root/explorer"}),
		`{"op":"message","id":"s1","sourceId":"s1","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"s1","name":"spawn_agent","input":{"agent_path":"/root/explorer"}}]}`,
		`{"op":"result","id":"s1","sourceId":"s1","ts":`+codexItemDoneTS+`,"toolUseId":"s1","result":{"output":"","status":"success","agent":{"id":"th3"}}}`)

	// Started without a thread ID: the spawn, no agent.
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "s3", Kind: codexitem.KindSubAgent, ActivityKind: "started", AgentPath: "/root/b"}),
		`{"op":"message","id":"s3","sourceId":"s3","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"s3","name":"spawn_agent","input":{"agent_path":"/root/b"}}]}`,
		`{"op":"result","id":"s3","sourceId":"s3","ts":`+codexItemDoneTS+`,"toolUseId":"s3","result":{"output":"","status":"success"}}`)

	// Coordination with a running sub-agent shows as the call it was.
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "s4", Kind: codexitem.KindSubAgent, ActivityKind: "interacted", AgentThreadID: "th3", AgentPath: "/root/explorer"}),
		`{"op":"message","id":"s4","sourceId":"s4","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"s4","name":"interact_agent","input":{"agent_path":"/root/explorer"}}]}`,
		`{"op":"result","id":"s4","sourceId":"s4","ts":`+codexItemDoneTS+`,"toolUseId":"s4","result":{"output":"","status":"success","agent":{"id":"th3"}}}`)
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "s5", Kind: codexitem.KindSubAgent, ActivityKind: "interrupted", AgentThreadID: "th3", AgentPath: "/root/explorer"}),
		`{"op":"message","id":"s5","sourceId":"s5","role":"assistant","ts":`+codexItemDoneTS+`,"content":[{"type":"tool_use","id":"s5","name":"interrupt_agent","input":{"agent_path":"/root/explorer"}}]}`,
		`{"op":"result","id":"s5","sourceId":"s5","ts":`+codexItemDoneTS+`,"toolUseId":"s5","result":{"output":"","status":"success","agent":{"id":"th3"}}}`)

	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "s2", Kind: codexitem.KindSubAgent, ActivityKind: "completed", AgentThreadID: "th3"}))
}

func TestCodexItemRecordsOtherHasNone(t *testing.T) {
	t.Parallel()
	assertCodexRecords(t, doneCodexItem(codexitem.Item{ID: "r1", Kind: codexitem.KindOther}))
}

func TestCodexItemRecordsTimestamp(t *testing.T) {
	t.Parallel()
	const line = "2030-01-01T00:00:01.400Z"
	cases := []struct {
		name               string
		status             codexitem.Status
		started, completed int64
		lineTS             string
		want               string
	}{
		{"done: completed time first", codexitem.StatusCompleted, codexItemStartMs, codexItemDoneMs, line, codexItemDoneTS},
		{"done: then the line's", codexitem.StatusCompleted, codexItemStartMs, 0, line, `"` + line + `"`},
		{"done: then the start", codexitem.StatusCompleted, codexItemStartMs, 0, "", codexItemStartTS},
		{"done: none", codexitem.StatusCompleted, 0, 0, "", ""},
		{"running: start first", codexitem.StatusInProgress, codexItemStartMs, codexItemDoneMs, line, codexItemStartTS},
		{"running: then the line's", codexitem.StatusInProgress, 0, codexItemDoneMs, line, `"` + line + `"`},
		{"running: never the completed time", codexitem.StatusInProgress, 0, codexItemDoneMs, "", ""},
	}
	for _, c := range cases {
		it := codexitem.Item{ID: "m1", Kind: codexitem.KindAgent, Text: "x", Status: c.status,
			StartedAtMs: c.started, CompletedAtMs: c.completed, LineTS: c.lineTS}
		recs := codexItemRecords(it)
		if len(recs) == 0 {
			t.Fatalf("%s: no records", c.name)
		}
		if got := string(recs[0].TS); got != c.want {
			t.Errorf("%s: ts = %s, want %s", c.name, got, c.want)
		}
	}
}

// Every kind's records fold without a validation error, and the started and
// completed records of a command fold to the completed line.
func TestCodexItemRecordsFold(t *testing.T) {
	t.Parallel()
	items := []codexitem.Item{
		{ID: "u1", Kind: codexitem.KindUser, Parts: []string{"hi"}},
		{ID: "m1", Kind: codexitem.KindAgent, Text: "Hello"},
		{ID: "e1", Kind: codexitem.KindCommand, Command: "ls", Status: codexitem.StatusInProgress},
		{ID: "e1", Kind: codexitem.KindCommand, Command: "ls", Output: "a\n", ExitCode: codexIntp(0)},
		{ID: "f1", Kind: codexitem.KindFileChange, Changes: []codexitem.Change{{Path: "/w/a", Kind: "add", Diff: "x\n"}}},
		{ID: "c1", Kind: codexitem.KindMCPTool, Server: "s", Tool: "t", Arguments: json.RawMessage(`{"a":1}`), Output: "ok"},
		{ID: "w1", Kind: codexitem.KindWebSearch, Query: "q"},
		{ID: "i1", Kind: codexitem.KindImageView, Path: "/w/p.png"},
		{ID: "k1", Kind: codexitem.KindCollabTool, Tool: "spawnAgent", Prompt: "p", ReceiverThreadIDs: []string{"th2"}},
		{ID: "s1", Kind: codexitem.KindSubAgent, ActivityKind: "started", AgentThreadID: "th3", AgentPath: "/root/a"},
		{ID: "r1", Kind: codexitem.KindOther},
	}
	f := NewFolder(Options{Agent: "codex", CLIVersion: "0.1.0"})
	for _, it := range items {
		if it.Status == "" {
			it = doneCodexItem(it)
		}
		if _, err := f.Apply(codexItemRecords(it)); err != nil {
			t.Fatalf("item %s (%s): %v", it.ID, it.Kind, err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(f.Lines()), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d lines, want 9 (reasoning has none):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	wantCommand := `{"v":1,"agent":"codex","cli_version":"0.1.0","type":"assistant","ts":` + codexItemDoneTS +
		`,"id":"e1","content":[{"id":"e1","input":{"command":"ls"},"name":"Bash","result":{"output":"a\n","status":"success"},"type":"tool_use"}]}`
	if lines[2] != wantCommand {
		t.Errorf("folded command line:\n got %s\nwant %s", lines[2], wantCommand)
	}
}

// codexKindCase is one item in both encodings and the Item both decode to, without
// the per-encoding fields (times, LineTS) and with TurnID "t1".
type codexKindCase struct {
	name    string
	rollout string // payload.item; empty when the case has no rollout form
	daemon  string // params.item of an item/completed; empty when it has no daemon form
	want    codexitem.Item
}

var codexKindCases = []codexKindCase{
	{
		name:    "user",
		rollout: `{"type":"UserMessage","id":"u1","client_id":"c","content":[{"type":"text","text":"a","text_elements":[]},{"type":"image","image_url":"data:x"},{"type":"mention","name":"m","path":"/p","text":"junk"},{"type":"text","text":"b"}]}`,
		daemon:  `{"type":"userMessage","id":"u1","clientId":"c","content":[{"type":"text","text":"a","text_elements":[]},{"type":"image","url":"data:x"},{"type":"skill","name":"s","path":"/p","text":"junk"},{"type":"text","text":"b"}]}`,
		want:    codexitem.Item{ID: "u1", Kind: codexitem.KindUser, Status: codexitem.StatusCompleted, Parts: []string{"a", "b"}},
	},
	{
		name:    "agent",
		rollout: `{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"Hel"},{"type":"Text","text":"lo"}],"phase":"final_answer"}`,
		daemon:  `{"type":"agentMessage","id":"m1","text":"Hello","phase":"final_answer","memoryCitation":null}`,
		want:    codexitem.Item{ID: "m1", Kind: codexitem.KindAgent, Status: codexitem.StatusCompleted, Text: "Hello"},
	},
	{
		name:    "command",
		rollout: `{"type":"CommandExecution","id":"e1","command":["/bin/zsh","-lc","ls -la"],"cwd":"file:///work/my%20repo","status":"failed","aggregated_output":"out\n","exit_code":1,"parsed_cmd":[]}`,
		daemon:  `{"type":"commandExecution","id":"e1","command":"/bin/zsh -lc 'ls -la'","cwd":"/work/my repo","status":"failed","aggregatedOutput":"out\n","exitCode":1,"commandActions":[]}`,
		want:    codexitem.Item{ID: "e1", Kind: codexitem.KindCommand, Status: codexitem.StatusFailed, Command: "ls -la", CWD: "/work/my repo", Output: "out\n", ExitCode: codexIntp(1)},
	},
	{
		name:    "fileChange add and update with a move",
		rollout: `{"type":"FileChange","id":"f1","changes":{"/w/b.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-a\n+b\n","move_path":"/w/c.go"},"/w/a.txt":{"type":"add","content":"hi\n"}},"status":"completed","stdout":"Success.","stderr":""}`,
		daemon:  `{"type":"fileChange","id":"f1","changes":[{"path":"/w/b.go","kind":{"type":"update","move_path":"/w/c.go"},"diff":"@@ -1 +1 @@\n-a\n+b\n"},{"path":"/w/a.txt","kind":{"type":"add"},"diff":"hi\n"}],"status":"completed"}`,
		want: codexitem.Item{ID: "f1", Kind: codexitem.KindFileChange, Status: codexitem.StatusCompleted, Changes: []codexitem.Change{
			{Path: "/w/a.txt", Kind: "add", Diff: "hi\n"},
			{Path: "/w/b.go", Kind: "update", MovePath: "/w/c.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		}},
	},
	{
		name:    "command, a non-shell program with a quoted argument",
		rollout: `{"type":"CommandExecution","id":"e2","command":["grep","-c","two words","notes.txt"],"cwd":"file:///work/repo","status":"completed","aggregated_output":"1\n","exit_code":0}`,
		daemon:  `{"type":"commandExecution","id":"e2","command":"grep -c 'two words' notes.txt","cwd":"/work/repo","status":"completed","aggregatedOutput":"1\n","exitCode":0}`,
		want:    codexitem.Item{ID: "e2", Kind: codexitem.KindCommand, Status: codexitem.StatusCompleted, Command: "grep -c two words notes.txt", CWD: "/work/repo", Output: "1\n", ExitCode: codexIntp(0)},
	},
	{
		name:    "command, a multi-line shell script the daemon quotes ANSI-C style",
		rollout: `{"type":"CommandExecution","id":"e3","command":["/bin/bash","-lc","cat <<EOF\nit's\nEOF"],"cwd":"file:///work/repo","status":"completed","aggregated_output":"it's\n","exit_code":0}`,
		daemon:  `{"type":"commandExecution","id":"e3","command":"/bin/bash -lc $'cat <<EOF\\nit\\'s\\nEOF'","cwd":"/work/repo","status":"completed","aggregatedOutput":"it's\n","exitCode":0}`,
		want:    codexitem.Item{ID: "e3", Kind: codexitem.KindCommand, Status: codexitem.StatusCompleted, Command: "cat <<EOF\nit's\nEOF", CWD: "/work/repo", Output: "it's\n", ExitCode: codexIntp(0)},
	},
	{
		// A daemon string shellWords can't split (an unterminated quote) is
		// kept whole; the argv it stands for joins to the same text.
		name:    "command the daemon string can't be split",
		rollout: `{"type":"CommandExecution","id":"e4","command":["echo","'x"],"cwd":"file:///work/repo","status":"failed","aggregated_output":"","exit_code":2}`,
		daemon:  `{"type":"commandExecution","id":"e4","command":"echo 'x","cwd":"/work/repo","status":"failed","aggregatedOutput":"","exitCode":2}`,
		want:    codexitem.Item{ID: "e4", Kind: codexitem.KindCommand, Status: codexitem.StatusFailed, Command: "echo 'x", CWD: "/work/repo", ExitCode: codexIntp(2)},
	},
	{
		name:    "fileChange delete",
		rollout: `{"type":"FileChange","id":"f3","changes":{"/w/old.txt":{"type":"delete","content":"bye\n"}},"status":"completed"}`,
		daemon:  `{"type":"fileChange","id":"f3","changes":[{"path":"/w/old.txt","kind":{"type":"delete"},"diff":"bye\n"}],"status":"completed"}`,
		want: codexitem.Item{ID: "f3", Kind: codexitem.KindFileChange, Status: codexitem.StatusCompleted, Changes: []codexitem.Change{
			{Path: "/w/old.txt", Kind: "delete", Diff: "bye\n"},
		}},
	},
	{
		name:    "fileChange update in place",
		rollout: `{"type":"FileChange","id":"f2","changes":{"/w/b.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-a\n+b\n","move_path":null}},"status":"completed"}`,
		daemon:  `{"type":"fileChange","id":"f2","changes":[{"path":"/w/b.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-a\n+b\n"}],"status":"completed"}`,
		want: codexitem.Item{ID: "f2", Kind: codexitem.KindFileChange, Status: codexitem.StatusCompleted, Changes: []codexitem.Change{
			{Path: "/w/b.go", Kind: "update", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		}},
	},
	{
		name:    "mcpTool success",
		rollout: `{"type":"McpToolCall","id":"c1","server":"test_server","tool":"echo","arguments":{ "text" : "hi" },"status":"completed","result":{"content":[{"type":"text","text":"4"},{"type":"image","data":"x"},{"type":"text","text":"!"}],"isError":false},"error":null}`,
		daemon:  `{"type":"mcpToolCall","id":"c1","server":"test_server","tool":"echo","arguments":{"text":"hi"},"status":"completed","result":{"content":[{"type":"text","text":"4"},{"type":"image","data":"x"},{"type":"text","text":"!"}]},"error":null}`,
		want:    codexitem.Item{ID: "c1", Kind: codexitem.KindMCPTool, Status: codexitem.StatusCompleted, Server: "test_server", Tool: "echo", Arguments: json.RawMessage(`{"text":"hi"}`), Output: "4!"},
	},
	{
		name:    "mcpTool failed with an error",
		rollout: `{"type":"McpToolCall","id":"c2","server":"s","tool":"t","arguments":null,"status":"failed","error":{"message":"denied"}}`,
		daemon:  `{"type":"mcpToolCall","id":"c2","server":"s","tool":"t","arguments":null,"status":"failed","result":null,"error":{"message":"denied"}}`,
		want:    codexitem.Item{ID: "c2", Kind: codexitem.KindMCPTool, Status: codexitem.StatusFailed, Server: "s", Tool: "t", Error: "denied"},
	},
	{
		name:    "mcpTool result isError without a message",
		rollout: `{"type":"McpToolCall","id":"c3","server":"s","tool":"t","status":"completed","result":{"content":[{"type":"text","text":"boom"}],"isError":true}}`,
		want:    codexitem.Item{ID: "c3", Kind: codexitem.KindMCPTool, Status: codexitem.StatusCompleted, Server: "s", Tool: "t", Output: "boom", Error: "tool reported an error"},
	},
	{
		name:    "webSearch",
		rollout: `{"type":"Extension","kind":"web.search","id":"w1","query":"example query","action":{"type":"search","query":"example query","queries":null},"results":[]}`,
		daemon:  `{"type":"webSearch","id":"w1","query":"example query","action":{"type":"search","query":"example query","queries":null},"results":[]}`,
		want:    codexitem.Item{ID: "w1", Kind: codexitem.KindWebSearch, Status: codexitem.StatusCompleted, Query: "example query"},
	},
	{
		name:    "imageView",
		rollout: `{"type":"ImageView","id":"i1","path":"/w/shot.png"}`,
		daemon:  `{"type":"imageView","id":"i1","path":"/w/shot.png"}`,
		want:    codexitem.Item{ID: "i1", Kind: codexitem.KindImageView, Status: codexitem.StatusCompleted, Path: "/w/shot.png"},
	},
	{
		name:    "collabTool spawn",
		rollout: `{"type":"CollabAgentToolCall","id":"k1","tool":"spawnAgent","status":"completed","prompt":"look into it","receiver_thread_ids":["th2","th3"]}`,
		daemon:  `{"type":"collabAgentToolCall","id":"k1","tool":"spawnAgent","status":"completed","prompt":"look into it","receiverThreadIds":["th2","th3"]}`,
		want:    codexitem.Item{ID: "k1", Kind: codexitem.KindCollabTool, Status: codexitem.StatusCompleted, Tool: "spawnAgent", Prompt: "look into it", ReceiverThreadIDs: []string{"th2", "th3"}},
	},
	{
		name:    "subAgentActivity",
		rollout: `{"type":"SubAgentActivity","id":"s1","kind":"started","agent_thread_id":"th2","agent_path":"/root/explorer"}`,
		daemon:  `{"type":"subAgentActivity","id":"s1","kind":"started","agentThreadId":"th2","agentPath":"/root/explorer"}`,
		want:    codexitem.Item{ID: "s1", Kind: codexitem.KindSubAgent, Status: codexitem.StatusCompleted, AgentThreadID: "th2", AgentPath: "/root/explorer", ActivityKind: "started"},
	},
	{
		name:    "subAgentActivity interacted",
		rollout: `{"type":"SubAgentActivity","id":"s4","kind":"interacted","agent_thread_id":"th2","agent_path":"/root/explorer"}`,
		daemon:  `{"type":"subAgentActivity","id":"s4","kind":"interacted","agentThreadId":"th2","agentPath":"/root/explorer"}`,
		want:    codexitem.Item{ID: "s4", Kind: codexitem.KindSubAgent, Status: codexitem.StatusCompleted, AgentThreadID: "th2", AgentPath: "/root/explorer", ActivityKind: "interacted"},
	},
	{
		name:    "subAgentActivity interrupted",
		rollout: `{"type":"SubAgentActivity","id":"s5","kind":"interrupted","agent_thread_id":"th2","agent_path":"/root/explorer"}`,
		daemon:  `{"type":"subAgentActivity","id":"s5","kind":"interrupted","agentThreadId":"th2","agentPath":"/root/explorer"}`,
		want:    codexitem.Item{ID: "s5", Kind: codexitem.KindSubAgent, Status: codexitem.StatusCompleted, AgentThreadID: "th2", AgentPath: "/root/explorer", ActivityKind: "interrupted"},
	},
	{
		name:    "reasoning",
		rollout: `{"type":"Reasoning","id":"r1","summary_text":[],"raw_content":[]}`,
		daemon:  `{"type":"reasoning","id":"r1","summary":[],"content":["thinking"]}`,
		want:    codexitem.Item{ID: "r1", Kind: codexitem.KindOther, Status: codexitem.StatusCompleted},
	},
	{
		name:    "other extension",
		rollout: `{"type":"Extension","kind":"clock.sleep","id":"x1","seconds":3}`,
		want:    codexitem.Item{ID: "x1", Kind: codexitem.KindOther, Status: codexitem.StatusCompleted},
	},
	{
		name:   "other daemon type",
		daemon: `{"type":"contextCompaction","id":"x2"}`,
		want:   codexitem.Item{ID: "x2", Kind: codexitem.KindOther, Status: codexitem.StatusCompleted},
	},
}

// TestCodexItemBothEncodings decodes each case's rollout and app-server forms
// to the same Item, and checks that the live mapping of the app-server form
// (CodexNotificationRecords) gives the line Convert gives for the rollout
// form.
func TestCodexItemBothEncodings(t *testing.T) {
	t.Parallel()
	const lineTS = "2030-01-01T00:00:00.745Z"
	for _, c := range codexKindCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rolloutLine := `{"timestamp":"` + lineTS + `","type":"event_msg","payload":{"type":"item_completed","thread_id":"th1","turn_id":"t1","item":` +
				c.rollout + `,"started_at_ms":100,"completed_at_ms":200}}`
			params := []byte(`{"item":` + c.daemon + `,"threadId":"th1","turnId":"t1","completedAtMs":200}`)
			if c.rollout != "" {
				want := c.want
				want.TurnID, want.StartedAtMs, want.CompletedAtMs, want.LineTS = "t1", 100, 200, lineTS
				if got, ok := codexitem.DecodeRollout([]byte(rolloutLine)); !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("DecodeRollout = %+v, %v\nwant %+v", got, ok, want)
				}
			}
			if c.daemon != "" {
				want := c.want
				want.TurnID, want.CompletedAtMs = "t1", 200
				if got, ok := codexitem.DecodeDaemon("item/completed", params); !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("DecodeDaemon = %+v, %v\nwant %+v", got, ok, want)
				}
			}
			if c.rollout == "" || c.daemon == "" {
				return
			}
			// Stored: the item in a one-turn rollout that a user message opens
			// (so Convert takes the item path); its lines after the user's.
			rollout := `{"timestamp":"t","type":"session_meta","payload":{"id":"th1"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u0","content":[{"type":"text","text":"go"}]}}}
` + rolloutLine + "\n"
			full, err := Convert([]byte(rollout), codexItemsOpts)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			_, stored, _ := bytes.Cut(full, []byte("\n"))
			recs, ok := CodexNotificationRecords("item/completed", params, CodexLive{})
			if !ok {
				t.Fatal("CodexNotificationRecords: ok = false")
			}
			live, err := Fold(recs, codexItemsOpts)
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			if !bytes.Equal(live, stored) {
				t.Errorf("live and stored lines differ\nlive:\n%s\nstored:\n%s", live, stored)
			}
		})
	}
}

func TestCodexNotificationRecordsLive(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	started := func(item string) []byte {
		return []byte(`{"item":` + item + `,"threadId":"th1","turnId":"t1","startedAtMs":` + strconv.FormatInt(codexItemStartMs, 10) + `}`)
	}
	agent := started(`{"type":"agentMessage","id":"m1","text":""}`)
	command := started(`{"type":"commandExecution","id":"e1","command":"/bin/zsh -lc 'make test'","cwd":"/w","status":"inProgress","aggregatedOutput":null,"exitCode":null}`)
	use := `{"op":"message","id":"e1","sourceId":"e1","role":"assistant","ts":` + codexItemStartTS + `,"content":[{"type":"tool_use","id":"e1","name":"Bash","input":{"command":"make test"}}]}`
	cases := []struct {
		name   string
		params []byte
		live   CodexLive
		want   []string
	}{
		{"agent before any delta", agent, CodexLive{}, nil},
		{"agent text so far", agent, CodexLive{Text: str("Hel")},
			[]string{`{"op":"message","id":"m1","sourceId":"m1","role":"assistant","ts":` + codexItemStartTS + `,"content":[{"type":"text","text":"Hel"}]}`}},
		{"running command", command, CodexLive{}, []string{use}},
		{"command output so far", command, CodexLive{Output: str("ok 1\n")}, []string{use,
			`{"op":"result","id":"e1","sourceId":"e1","ts":` + codexItemStartTS + `,"toolUseId":"e1","result":{"output":"ok 1\n","status":"success"}}`}},
		{"interrupted command", command, CodexLive{Output: str("ok 1\n"), Interrupted: true}, []string{
			`{"op":"message","id":"e1","sourceId":"e1","role":"assistant","ts":` + codexItemStartTS + `,"content":[{"type":"tool_use","id":"e1","name":"Bash","input":{"command":"make test"}}]}`,
			`{"op":"result","id":"e1","sourceId":"e1","ts":` + codexItemStartTS + `,"toolUseId":"e1","result":{"output":"ok 1\n","status":"error"}}`}},
		{"text override ignored for a command", command, CodexLive{Text: str("x")}, []string{use}},
		{"fallback time", []byte(`{"item":{"type":"agentMessage","id":"m2","text":"Hi"},"turnId":"t1"}`), CodexLive{TS: "2030-01-01T00:00:01.400Z"},
			[]string{`{"op":"message","id":"m2","sourceId":"m2","role":"assistant","ts":"2030-01-01T00:00:01.400Z","content":[{"type":"text","text":"Hi"}]}`}},
	}
	for _, c := range cases {
		method := "item/started"
		if c.name == "fallback time" {
			method = "item/completed"
		}
		recs, ok := CodexNotificationRecords(method, c.params, c.live)
		if !ok {
			t.Fatalf("%s: ok = false", c.name)
		}
		if got, want := codexRecordLines(t, recs), strings.Join(c.want, "\n"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
	}
	// A completed item's status is final: Interrupted changes nothing.
	done := []byte(`{"item":{"type":"commandExecution","id":"e2","command":"ls","cwd":"/w","status":"completed","aggregatedOutput":"a\n","exitCode":0},"turnId":"t1"}`)
	recs, _ := CodexNotificationRecords("item/completed", done, CodexLive{Interrupted: true})
	if len(recs) != 2 || recs[1].Result.Status != ToolStatusSuccess {
		t.Errorf("completed command with Interrupted: %s", codexRecordLines(t, recs))
	}
}
