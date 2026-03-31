package compact

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

var defaultOpts = MetadataFields{
	Agent:      "claude-code",
	CLIVersion: "0.5.1",
	StartLine:  0,
}

func agentOpts(agent string) MetadataFields {
	return MetadataFields{
		Agent:      agent,
		CLIVersion: "0.5.1",
		StartLine:  0,
	}
}

// --- Claude Code tests ---

func TestCompact_SimpleConversation(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","parentUuid":"","cwd":"/repo","message":{"content":"hello"}}
{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"text","text":"Hi!"}],"usage":{"input_tokens":100,"output_tokens":50}}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"msg-1","input_tokens":100,"output_tokens":50,"content":[{"type":"text","text":"Hi!"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_AssistantStripping(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"thinking","thinking":"hmm..."},{"type":"redacted_thinking","data":"secret"},{"type":"text","text":"Here's my answer."},{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"},"caller":"internal"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"msg-1","content":[{"type":"text","text":"Here's my answer."},{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_AssistantThinkingOnly(t *testing.T) {
	t.Parallel()

	// Assistant lines with only thinking content should be dropped entirely
	// (streaming intermediates that carry no user-visible content).
	input := []byte(`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"thinking","thinking":"hmm..."}]}}
`)

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, nil)
}

func TestCompact_UserWithToolResult(t *testing.T) {
	t.Parallel()

	// Assistant with tool_use followed by user with tool_result: the result
	// is inlined into the assistant's tool_use block and the user tool_result
	// line is dropped. Rich metadata from toolUseResult (file) is preserved.
	input := []byte(`{"type":"assistant","timestamp":"2026-01-01T00:00:59Z","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"u2","timestamp":"2026-01-01T00:01:00Z","parentUuid":"u1","cwd":"/repo","sessionId":"sess-1","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"file1.txt\nfile2.txt"},{"type":"text","text":"now fix the bug"}]},"toolUseResult":{"type":"text","file":{"filePath":"/repo/file1.txt","numLines":10},"output":"file1.txt\nfile2.txt","matchCount":2}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:59Z","id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"},"result":{"output":"file1.txt\nfile2.txt","status":"success","file":{"filePath":"/repo/file1.txt","numLines":10}}}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:01:00Z","content":[{"text":"now fix the bug"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_UserWithMultipleToolResults(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"assistant","timestamp":"2026-01-01T00:00:59Z","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"ReadFile","input":{"path":"a.txt"}},{"type":"tool_use","id":"tu-2","name":"ReadFile","input":{"path":"b.txt"}}]}}
{"type":"user","uuid":"u2","timestamp":"2026-01-01T00:01:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"A"},{"type":"tool_result","tool_use_id":"tu-2","content":"B"},{"type":"text","text":"continue"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:59Z","id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"ReadFile","input":{"path":"a.txt"},"result":{"output":"A","status":"success"}},{"type":"tool_use","id":"tu-2","name":"ReadFile","input":{"path":"b.txt"},"result":{"output":"B","status":"success"}}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:01:00Z","content":[{"text":"continue"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_UserNoText(t *testing.T) {
	t.Parallel()

	// User entry with only tool_result (no text) preceded by assistant with tool_use:
	// result is inlined and user line is dropped entirely (no text content).
	input := []byte(`{"type":"assistant","timestamp":"t0","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"echo done"}}]}}
{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"done"}]},"toolUseResult":{"stdout":"done","stderr":""}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t0","id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"echo done"},"result":{"output":"done","status":"success"}}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_AssistantStringContent(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"assistant","timestamp":"t1","requestId":"r1","message":{"id":"m1","content":"just a string"}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t1","id":"m1","content":"just a string"}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_HumanTypeAlias(t *testing.T) {
	t.Parallel()

	// Claude Code transcripts may use type:"human" for user messages.
	input := []byte(`{"type":"human","timestamp":"2026-01-01T00:00:00Z","message":{"content":"hello human"}}
{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","message":{"id":"m1","content":[{"type":"text","text":"Hi!"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"hello human"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"m1","content":[{"type":"text","text":"Hi!"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_ClaudeFixture(t *testing.T) {
	t.Parallel()

	assertFixtureTransform(t, defaultOpts, "testdata/claude_full.jsonl", "testdata/claude_expected.jsonl")
}

func TestCompact_ClaudeFixture2(t *testing.T) {
	t.Parallel()

	assertFixtureTransform(t, defaultOpts, "testdata/claude_full2.jsonl", "testdata/claude_expected2.jsonl")
}

// --- Token usage tests ---

func TestCompact_AssistantTokenUsage(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"user","uuid":"u1","timestamp":"t0","message":{"content":"hello"}}
{"type":"assistant","timestamp":"t1","requestId":"r1","message":{"id":"m1","content":[{"type":"text","text":"Hi!"}],"usage":{"input_tokens":200,"output_tokens":75}}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"t0","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t1","id":"m1","input_tokens":200,"output_tokens":75,"content":[{"type":"text","text":"Hi!"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_StreamingFragmentTokenMerge(t *testing.T) {
	t.Parallel()

	// Two streaming fragments of the same message; the later one has the final token counts.
	input := []byte(`{"type":"assistant","timestamp":"t1","requestId":"r1","message":{"id":"m1","content":[{"type":"thinking","thinking":"hmm"}],"usage":{"input_tokens":100,"output_tokens":5}}}
{"type":"assistant","timestamp":"t2","requestId":"r1","message":{"id":"m1","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":100,"output_tokens":42}}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t2","id":"m1","input_tokens":100,"output_tokens":42,"content":[{"type":"text","text":"done"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_NoUsageOmitsTokenFields(t *testing.T) {
	t.Parallel()

	// Assistant without usage: input_tokens and output_tokens should be omitted.
	input := []byte(`{"type":"assistant","timestamp":"t1","requestId":"r1","message":{"id":"m1","content":[{"type":"text","text":"Hi!"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t1","id":"m1","content":[{"type":"text","text":"Hi!"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// --- Rich tool result metadata tests ---

func TestCompact_ReadToolResult_PreservesFileMetadata(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"assistant","timestamp":"t0","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/repo/main.go"}}]}}
{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"package main\nfunc main() {}"}]},"toolUseResult":{"type":"text","file":{"filePath":"/repo/main.go","numLines":2,"startLine":1,"totalLines":2,"content":"package main\nfunc main() {}"}}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t0","id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/repo/main.go"},"result":{"output":"package main\nfunc main() {}","status":"success","file":{"filePath":"/repo/main.go","numLines":2}}}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_GrepToolResult_PreservesMatchCount(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"assistant","timestamp":"t0","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Grep","input":{"pattern":"TODO"}}]}}
{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"Found 5 files\na.go\nb.go"}]},"toolUseResult":{"content":"Found 5 files\na.go\nb.go","numFiles":5,"numLines":10,"filenames":["a.go","b.go"],"mode":"files_with_matches"}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t0","id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Grep","input":{"pattern":"TODO"},"result":{"output":"Found 5 files\na.go\nb.go","status":"success","matchCount":5}}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_EditToolResult_PreservesFilePath(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"assistant","timestamp":"t0","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Edit","input":{"file_path":"/repo/main.go","old_string":"bad","new_string":"good"}}]}}
{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":""}]},"toolUseResult":{"filePath":"/repo/main.go","oldString":"bad","newString":"good","structuredPatch":"@@ -1 +1 @@\n-bad\n+good"}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t0","id":"msg-1","content":[{"type":"tool_use","id":"tu-1","name":"Edit","input":{"file_path":"/repo/main.go","old_string":"bad","new_string":"good"},"result":{"output":"","status":"success","file":{"filePath":"/repo/main.go"}}}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// --- Image tests ---

func TestCompact_UserWithImages(t *testing.T) {
	t.Parallel()

	tinyPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

	input := []byte(`{"type":"user","promptId":"p1","timestamp":"t1","message":{"content":[{"type":"text","text":"the footer should still show"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}}]}}
{"type":"assistant","timestamp":"t2","requestId":"r1","message":{"id":"m1","content":[{"type":"text","text":"I see the screenshots."}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"id":"p1","text":"the footer should still show"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t2","id":"m1","content":[{"type":"text","text":"I see the screenshots."}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_UserWithImageOnly(t *testing.T) {
	t.Parallel()

	tinyPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

	// User message with only an image and no text should still be emitted.
	input := []byte(`{"type":"user","timestamp":"t1","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// --- Truncation + filtering tests ---

// Realistic full.jsonl: lines 0-2 are duplicated prefix, lines 3-6 are new content.
var fixtureFullJSONL = strings.Join([]string{
	`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","parentUuid":"","cwd":"/repo","sessionId":"sess-1","version":"1","gitBranch":"main","message":{"content":"hello"}}`,
	`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"req-1","message":{"id":"msg-1","content":[{"type":"thinking","thinking":"let me think..."},{"type":"text","text":"Hi there!"},{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"},"caller":"some-caller"}]}}`,
	`{"type":"progress","message":{"type":"bash","content":"running..."}}`,
	`{"type":"user","uuid":"u2","timestamp":"2026-01-01T00:01:00Z","parentUuid":"u1","cwd":"/repo","sessionId":"sess-1","version":"1","gitBranch":"main","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"file1.txt\nfile2.txt"},{"type":"text","text":"now fix the bug"}]},"toolUseResult":{"type":"text","file":{"filePath":"/repo/file1.txt","numLines":10},"output":"file1.txt\nfile2.txt","matchCount":2}}`,
	`{"type":"assistant","timestamp":"2026-01-01T00:01:01Z","requestId":"req-2","message":{"id":"msg-2","content":[{"type":"thinking","thinking":"analyzing the bug..."},{"type":"redacted_thinking","data":"abc123"},{"type":"text","text":"I found the issue."},{"type":"tool_use","id":"tu-2","name":"Edit","input":{"file_path":"/repo/bug.go","old_string":"bad","new_string":"good"},"caller":"internal"}]}}`,
	`{"type":"file-history-snapshot","files":["/repo/bug.go"]}`,
	`{"type":"system","message":{"content":"system reminder"}}`,
}, "\n") + "\n"

func TestCompact_FullFixture_WithTruncation(t *testing.T) {
	t.Parallel()

	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 3}

	// Starting at line 3 (user with tool_result), there's no preceding assistant
	// to inline into, so user text is emitted and tool result is lost.
	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:01:00Z","content":[{"text":"now fix the bug"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:01:01Z","id":"msg-2","content":[{"type":"text","text":"I found the issue."},{"type":"tool_use","id":"tu-2","name":"Edit","input":{"file_path":"/repo/bug.go","old_string":"bad","new_string":"good"}}]}`,
	}

	result, err := Compact([]byte(fixtureFullJSONL), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_FullFixture_NoTruncation(t *testing.T) {
	t.Parallel()

	expected := []string{
		// Line 0: user "hello"
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"hello"}]}`,
		// Line 1: assistant (thinking stripped, caller stripped, tool result inlined from line 3 with file metadata)
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"msg-1","content":[{"type":"text","text":"Hi there!"},{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"},"result":{"output":"file1.txt\nfile2.txt","status":"success","file":{"filePath":"/repo/file1.txt","numLines":10}}}]}`,
		// Line 2: progress — dropped
		// Line 3: user with tool_result — inlined above, user text emitted
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:01:00Z","content":[{"text":"now fix the bug"}]}`,
		// Line 4: assistant (thinking + redacted_thinking stripped)
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:01:01Z","id":"msg-2","content":[{"type":"text","text":"I found the issue."},{"type":"tool_use","id":"tu-2","name":"Edit","input":{"file_path":"/repo/bug.go","old_string":"bad","new_string":"good"}}]}`,
		// Lines 5-6: file-history-snapshot, system — dropped
	}

	result, err := Compact([]byte(fixtureFullJSONL), defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// --- Field order test (exact byte comparison since struct field order is deterministic) ---

func TestCompact_FieldOrder(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"content":"hello"}}
`)

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := `{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"hello"}]}` + "\n"
	if string(result) != expected {
		t.Errorf("field order mismatch:\ngot:  %s\nwant: %s", string(result), expected)
	}
}

// --- Cursor tests ---

func TestCompact_CursorRoleOnly(t *testing.T) {
	t.Parallel()

	cursorOpts := agentOpts("cursor")

	// Cursor transcripts use "role" instead of "type".
	input := []byte(`{"role":"user","timestamp":"t1","message":{"content":"hello from cursor"}}
{"role":"assistant","timestamp":"t2","message":{"content":[{"type":"text","text":"Hi from Cursor!"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"hello from cursor"}]}`,
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"assistant","ts":"t2","content":[{"type":"text","text":"Hi from Cursor!"}]}`,
	}

	result, err := Compact(input, cursorOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_StripsIDEContextTags(t *testing.T) {
	t.Parallel()

	// User text with Cursor's <user_query> tags should have tags stripped.
	input := []byte(`{"role":"user","timestamp":"t1","message":{"content":"<user_query>\nhello world\n</user_query>"}}
`)

	cursorOpts := agentOpts("cursor")

	expected := []string{
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"hello world"}]}`,
	}

	result, err := Compact(input, cursorOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_StripsIDEContextTagsFromContentBlocks(t *testing.T) {
	t.Parallel()

	// Array content with IDE tags in text blocks.
	input := []byte(`{"type":"user","timestamp":"t1","message":{"content":[{"type":"text","text":"<user_query>\nfix the bug\n</user_query>"},{"type":"text","text":"also this"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"fix the bug\n\nalso this"}]}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// --- Cross-agent format tests ---

func TestCompact_MixedFormats(t *testing.T) {
	t.Parallel()

	cursorOpts := agentOpts("cursor")

	// Mixed transcript: type-based Claude entries, role-based Cursor entries, and human alias.
	input := []byte(`{"type":"user","timestamp":"t1","message":{"content":"claude user"}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","content":[{"type":"text","text":"claude assistant"}]}}
{"role":"user","timestamp":"t3","message":{"content":"cursor user"}}
{"role":"assistant","timestamp":"t4","message":{"content":[{"type":"text","text":"cursor assistant"}]}}
{"type":"human","timestamp":"t5","message":{"content":"human alias"}}
{"type":"progress","message":{"content":"should be dropped"}}
`)

	expected := []string{
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"claude user"}]}`,
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"assistant","ts":"t2","id":"m1","content":[{"type":"text","text":"claude assistant"}]}`,
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"user","ts":"t3","content":[{"text":"cursor user"}]}`,
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"assistant","ts":"t4","content":[{"type":"text","text":"cursor assistant"}]}`,
		`{"v":1,"agent":"cursor","cli_version":"0.5.1","type":"user","ts":"t5","content":[{"text":"human alias"}]}`,
	}

	result, err := Compact(input, cursorOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_EmptyInput(t *testing.T) {
	t.Parallel()

	result, err := Compact([]byte{}, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, nil)
}

func TestCompact_StartLineBeyondEnd(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":"hello"}}
`)
	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 100}

	result, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, nil)
}

func TestCompact_MalformedLinesSkipped(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":"hello"}}
not valid json at all
{"type":"assistant","timestamp":"t2","requestId":"r1","message":{"id":"m1","content":"hi"}}
`)

	expected := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"t2","id":"m1","content":"hi"}`,
	}

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_OnlyDroppedTypes(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"progress","message":{"content":"..."}}
{"type":"file-history-snapshot","files":[]}
{"type":"queue-operation","op":"enqueue"}
{"type":"system","message":{"content":"reminder"}}
`)

	result, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, nil)
}

// --- Helpers ---

func assertFixtureTransform(t *testing.T, opts MetadataFields, inputPath, expectedPath string) {
	t.Helper()

	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("failed to read fixture %q: %v", inputPath, err)
	}

	expected, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("failed to read expected output %q: %v", expectedPath, err)
	}

	result, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, nonEmptyLines(expected))
}

func nonEmptyLines(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertJSONLines compares actual output lines against expected JSON strings,
// using semantic JSON equality (order-independent for object keys).
func assertJSONLines(t *testing.T, actual []byte, expected []string) {
	t.Helper()

	actualLines := nonEmptyLines(actual)

	if len(expected) == 0 && len(actualLines) == 0 {
		return
	}

	if len(actualLines) != len(expected) {
		t.Fatalf("line count mismatch: got %d, want %d\nactual:\n%s", len(actualLines), len(expected), string(actual))
	}

	for i := range expected {
		var got, want interface{}
		if err := json.Unmarshal([]byte(actualLines[i]), &got); err != nil {
			t.Fatalf("line %d: failed to parse actual JSON: %v\nline: %s", i, err, actualLines[i])
		}
		if err := json.Unmarshal([]byte(expected[i]), &want); err != nil {
			t.Fatalf("line %d: failed to parse expected JSON: %v\nline: %s", i, err, expected[i])
		}
		if !reflect.DeepEqual(got, want) {
			prettyGot, _ := json.MarshalIndent(got, "", "  ")   //nolint:errcheck,errchkjson // test helper, marshal of interface{} is best-effort
			prettyWant, _ := json.MarshalIndent(want, "", "  ") //nolint:errcheck,errchkjson // test helper, marshal of interface{} is best-effort
			t.Errorf("line %d mismatch:\ngot:\n%s\nwant:\n%s", i, prettyGot, prettyWant)
		}
	}
}
