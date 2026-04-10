package compact

import (
	"strings"
	"testing"
)

func TestCompact_CodexFixture(t *testing.T) {
	t.Parallel()

	codexOpts := agentOpts("codex")
	assertFixtureTransform(t, codexOpts, "testdata/codex_full.jsonl", "testdata/codex_expected.jsonl")
}

func TestCompact_CodexInlineCases(t *testing.T) {
	t.Parallel()

	codexOpts := agentOpts("codex")

	tests := []struct {
		name     string
		input    []byte
		expected []string
	}{
		{
			name: "user message with system content filtered",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions>sandbox</permissions>"}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md\ninstructions"},{"type":"input_text","text":"hello world"}]}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"t3","content":[{"text":"hello world"}]}`,
			},
		},
		{
			name: "assistant with function call and output",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running the command."}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}
{"timestamp":"t4","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file1.txt\nfile2.txt"}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t2","content":[{"type":"text","text":"Running the command."},{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"ls"},"result":{"output":"file1.txt\nfile2.txt","status":"success"}}]}`,
			},
		},
		{
			name: "assistant with multiple output text blocks",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first line"},{"type":"output_text","text":"second line"}]}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t2","content":[{"type":"text","text":"first line\n\nsecond line"}]}`,
			},
		},
		{
			name: "assistant with custom_tool_call apply_patch and output",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Creating the file now."}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+Hello World\n*** End Patch\n"}}
{"timestamp":"t4","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":{"type":"text","text":"Success. Updated: A hello.txt"}}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t2","content":[{"type":"text","text":"Creating the file now."},{"type":"tool_use","id":"call_1","name":"apply_patch","input":{"input":"*** Begin Patch\n*** Add File: hello.txt\n+Hello World\n*** End Patch\n"},"result":{"output":"Success. Updated: A hello.txt","status":"success"}}]}`,
			},
		},
		{
			name: "standalone custom_tool_call without preceding assistant text",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch\n*** Update File: readme.md\n-old\n+new\n*** End Patch\n"}}
{"timestamp":"t3","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":{"type":"text","text":"Success. Updated: M readme.md"}}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t2","content":[{"type":"tool_use","id":"call_2","name":"apply_patch","input":{"input":"*** Begin Patch\n*** Update File: readme.md\n-old\n+new\n*** End Patch\n"},"result":{"output":"Success. Updated: M readme.md","status":"success"}}]}`,
			},
		},
		{
			name: "mixed function_call and custom_tool_call after same assistant",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running then patching."}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}
{"timestamp":"t4","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file.txt"}}
{"timestamp":"t5","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"patch data"}}
{"timestamp":"t6","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":{"type":"text","text":"ok"}}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t2","content":[{"type":"text","text":"Running then patching."},{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"ls"},"result":{"output":"file.txt","status":"success"}},{"type":"tool_use","id":"call_2","name":"apply_patch","input":{"input":"patch data"},"result":{"output":"ok","status":"success"}}]}`,
			},
		},
		{
			name: "drops reasoning and event_msg",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"t3","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}
{"timestamp":"t4","type":"response_item","payload":{"type":"reasoning","summary":[]}}
{"timestamp":"t5","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"t3","content":[{"text":"hi"}]}`,
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t5","content":[{"type":"text","text":"hello"}]}`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := Compact(tc.input, codexOpts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertJSONLines(t, result, tc.expected)
		})
	}
}

func TestIsCodexFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{
			name:  "codex session_meta",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}` + "\n"),
			want:  true,
		},
		{
			name:  "claude code format",
			input: []byte(`{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":"hi"}}` + "\n"),
			want:  false,
		},
		{
			name:  "droid format",
			input: []byte(`{"type":"session_start","id":"s1"}` + "\n" + `{"type":"message","id":"m1","message":{"role":"user","content":"hi"}}` + "\n"),
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isCodexFormat(tc.input); got != tc.want {
				t.Errorf("isCodexFormat() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCodexFormat_LargeFirstLine(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 70*1024)
	input := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1","blob":"` + large + `"}}` + "\n")
	if got := isCodexFormat(input); !got {
		t.Fatalf("isCodexFormat() = %v, want true", got)
	}
}

func TestCompact_CodexStartLine(t *testing.T) {
	t.Parallel()

	// StartLine skips raw JSONL lines to match Codex CheckpointTranscriptStart.
	// StartLine=4 skips session_meta, developer, AGENTS.md user, and first prompt,
	// leaving the first assistant response and everything after it.
	opts := MetadataFields{Agent: "codex", CLIVersion: "0.5.1", StartLine: 4}

	input := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions>sandbox</permissions>"}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md\ninstructions"}]}}
{"timestamp":"t4","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}
{"timestamp":"t5","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"response to first"}]}}
{"timestamp":"t6","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second prompt"}]}}
{"timestamp":"t7","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"response to second"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t5","content":[{"type":"text","text":"response to first"}]}`,
		`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"t6","content":[{"text":"second prompt"}]}`,
		`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t7","content":[{"type":"text","text":"response to second"}]}`,
	}

	result, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_CodexStartLine_IgnoresTokenCountEvents(t *testing.T) {
	t.Parallel()

	// StartLine counts raw JSONL lines, so StartLine=2 skips session_meta and the
	// first user line, leaving the token_count event to attach to the assistant.
	opts := MetadataFields{Agent: "codex", CLIVersion: "0.5.1", StartLine: 2}

	input := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}
{"timestamp":"t3","type":"event_msg","payload":{"type":"token_count","input_tokens":10,"output_tokens":0}}
{"timestamp":"t4","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second entry"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t4","input_tokens":10,"content":[{"type":"text","text":"second entry"}]}`,
	}

	result, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}
