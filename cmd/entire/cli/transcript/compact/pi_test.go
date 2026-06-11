package compact

import (
	"strings"
	"testing"

	"github.com/entireio/cli/redact"
)

const piTestSessionJSONL = `{"type":"session","version":3,"id":"test-uuid-123","timestamp":"2026-03-27T21:00:00.000Z","cwd":"/tmp/test"}
{"type":"model_change","id":"mc1","parentId":null,"timestamp":"2026-03-27T21:00:00.001Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}
{"type":"message","id":"m1","parentId":"mc1","timestamp":"2026-03-27T21:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Create hello.txt"}],"timestamp":1774646400000}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-03-27T21:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"write","arguments":{"path":"hello.txt","content":"hello world\n"}}],"usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5},"stopReason":"toolUse","timestamp":1774646401000}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-03-27T21:00:03.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"write","content":[{"type":"text","text":"Written 12 bytes"}],"isError":false,"timestamp":1774646402000}}
{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-03-27T21:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Created hello.txt with the content hello world."}],"usage":{"input":200,"output":30,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":1774646403000}}
`

const piTestBranchingJSONL = `{"type":"session","version":3,"id":"test-branch-123","timestamp":"2026-03-27T22:00:00.000Z","cwd":"/tmp/test"}
{"type":"model_change","id":"mc1","parentId":null,"timestamp":"2026-03-27T22:00:00.001Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}
{"type":"message","id":"m1","parentId":"mc1","timestamp":"2026-03-27T22:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Create a file"}],"timestamp":1774650000000}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-03-27T22:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"write","arguments":{"path":"old.txt","content":"old\n"}}],"usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0},"stopReason":"toolUse","timestamp":1774650001000}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-03-27T22:00:03.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"write","content":[{"type":"text","text":"Written 4 bytes"}],"isError":false,"timestamp":1774650002000}}
{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-03-27T22:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Created old.txt"}],"usage":{"input":200,"output":30,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":1774650003000}}
{"type":"message","id":"m5","parentId":"m1","timestamp":"2026-03-27T22:00:05.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc2","name":"write","arguments":{"path":"new.txt","content":"new\n"}}],"usage":{"input":150,"output":60,"cacheRead":5,"cacheWrite":3},"stopReason":"toolUse","timestamp":1774650004000}}
{"type":"message","id":"m6","parentId":"m5","timestamp":"2026-03-27T22:00:06.000Z","message":{"role":"toolResult","toolCallId":"tc2","toolName":"write","content":[{"type":"text","text":"Written 4 bytes"}],"isError":false,"timestamp":1774650005000}}
{"type":"message","id":"m7","parentId":"m6","timestamp":"2026-03-27T22:00:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Created new.txt"}],"usage":{"input":250,"output":40,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":1774650006000}}
`

func TestCompact_Pi_LinearTranscript(t *testing.T) {
	t.Parallel()

	expected := []string{
		`{"v":1,"agent":"pi","cli_version":"0.5.1","type":"user","ts":"2026-03-27T21:00:01.000Z","content":[{"text":"Create hello.txt"}]}`,
		`{"v":1,"agent":"pi","cli_version":"0.5.1","type":"assistant","ts":"2026-03-27T21:00:02.000Z","id":"m2","input_tokens":100,"output_tokens":50,"content":[{"type":"tool_use","id":"tc1","name":"Write","input":{"content":"hello world\n","path":"hello.txt"},"result":{"output":"Written 12 bytes","status":"success"}}]}`,
		`{"v":1,"agent":"pi","cli_version":"0.5.1","type":"assistant","ts":"2026-03-27T21:00:04.000Z","id":"m4","input_tokens":200,"output_tokens":30,"content":[{"type":"text","text":"Created hello.txt with the content hello world."}]}`,
	}

	result, err := Compact(redact.AlreadyRedacted([]byte(piTestSessionJSONL)), agentOpts("pi"))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_Pi_FiltersAbandonedBranches(t *testing.T) {
	t.Parallel()

	result, err := Compact(redact.AlreadyRedacted([]byte(piTestBranchingJSONL)), agentOpts("pi"))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := string(result)
	if strings.Contains(got, "old.txt") || strings.Contains(got, "Created old.txt") {
		t.Errorf("compact should exclude abandoned branch (old.txt), got:\n%s", got)
	}
	if !strings.Contains(got, "new.txt") || !strings.Contains(got, "Created new.txt") {
		t.Errorf("compact should include active branch (new.txt), got:\n%s", got)
	}
}

func TestCompact_Pi_NormalizesToolNamesAndErrors(t *testing.T) {
	t.Parallel()
	body := []byte(`{"type":"session","version":3,"id":"s1","timestamp":"2026-03-27T21:00:00.000Z","cwd":"/r"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-03-27T21:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Edit app.go"}]}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-03-27T21:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"edit","arguments":{"path":"app.go"}}]}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-03-27T21:00:03.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"edit","content":[{"type":"text","text":"file not found"}],"isError":true}}
`)
	result, err := Compact(redact.AlreadyRedacted(body), agentOpts("pi"))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := string(result)
	if !strings.Contains(got, `"name":"Edit"`) {
		t.Errorf(`expected "name":"Edit" (normalized from "edit") in:\n%s`, got)
	}
	if !strings.Contains(got, `"status":"error"`) {
		t.Errorf(`expected "status":"error" (mapped from isError:true) in:\n%s`, got)
	}
}

func TestCompact_Pi_FlatTranscriptNoTreeFiltering(t *testing.T) {
	t.Parallel()
	body := []byte(`{"type":"session","version":3,"id":"flat"}
{"type":"message","id":"m1","timestamp":"2026-03-27T21:00:01.000Z","message":{"role":"user","content":"hello"}}
{"type":"message","id":"m2","timestamp":"2026-03-27T21:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input":10,"output":5}}}
`)
	expected := []string{
		`{"v":1,"agent":"pi","cli_version":"0.5.1","type":"user","ts":"2026-03-27T21:00:01.000Z","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"pi","cli_version":"0.5.1","type":"assistant","ts":"2026-03-27T21:00:02.000Z","id":"m2","input_tokens":10,"output_tokens":5,"content":[{"type":"text","text":"hi"}]}`,
	}
	result, err := Compact(redact.AlreadyRedacted(body), agentOpts("pi"))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// TestCompact_Pi_StartLineDoesNotLeakAbandonedBranches is a regression test
// for a bug found in review: an earlier version of compactPi truncated the
// content via SkipLines BEFORE resolving the active branch. When StartLine
// skipped past fork-point entries, ResolveActiveBranch ran on a disconnected
// tree, returned nil, and the loop fell back to "all entries are active" —
// letting abandoned-branch tool calls leak into the compact transcript.
func TestCompact_Pi_StartLineDoesNotLeakAbandonedBranches(t *testing.T) {
	t.Parallel()
	// Same shape as piTestBranchingJSONL, but bump StartLine past the
	// session header + model_change + m1 (fork point) — so that, on the
	// truncated buffer, ResolveActiveBranch cannot anchor to a root.
	opts := MetadataFields{Agent: "pi", CLIVersion: "0.5.1", StartLine: 3}
	result, err := Compact(redact.AlreadyRedacted([]byte(piTestBranchingJSONL)), opts)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := string(result)
	if strings.Contains(got, "old.txt") || strings.Contains(got, "Created old.txt") {
		t.Errorf("abandoned-branch leak: compact contains old.txt entries:\n%s", got)
	}
	if !strings.Contains(got, "new.txt") {
		t.Errorf("active-branch dropped: compact missing new.txt entries:\n%s", got)
	}
}

func TestCompact_Pi_StartLine(t *testing.T) {
	t.Parallel()
	// Skip header + model_change + first user → start scanning from m2.
	opts := MetadataFields{Agent: "pi", CLIVersion: "0.5.1", StartLine: 3}
	result, err := Compact(redact.AlreadyRedacted([]byte(piTestSessionJSONL)), opts)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := string(result)
	if strings.Contains(got, "Create hello.txt") {
		t.Errorf("StartLine=3 should skip user message, got:\n%s", got)
	}
	if !strings.Contains(got, `"id":"m2"`) || !strings.Contains(got, `"id":"m4"`) {
		t.Errorf("StartLine=3 should keep m2 and m4, got:\n%s", got)
	}
}

func TestIsPiFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"v3 header", `{"type":"session","version":3,"id":"x","cwd":"/"}`, true},
		{"v1 legacy", `{"type":"session","version":1,"id":"x"}`, true},
		{"opencode", `{"info":{"id":"x"},"messages":[]}`, false},
		{"claude jsonl", `{"type":"user","uuid":"u1","message":{"content":"hi"}}`, false},
		{"empty", "", false},
		{"garbage", "not-json", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := isPiFormat([]byte(c.input)); got != c.want {
				t.Errorf("isPiFormat(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
