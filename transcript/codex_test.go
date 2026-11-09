package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const cxPre = `{"v":1,"agent":"codex","cli_version":"0.5.1",`

func cxJoin(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func cxUser(ts, text string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, ts, text)
}

func cxMessage(ts, text string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, ts, text)
}

func cxReasoning(ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"reasoning","summary":[]}}`, ts)
}

func cxCall(ts, id string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"x\"}","call_id":%q}}`, ts, id)
}

func cxOutput(ts, id, out string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":%q}}`, ts, id, out)
}

func cxModel(model string) string {
	return fmt.Sprintf(`{"type":"turn_context","payload":{"model":%q}}`, model)
}

func cxTokens(ts string, input, cached, output int64, reasoning ...int64) string {
	r := ""
	if len(reasoning) > 0 {
		r = fmt.Sprintf(`,"reasoning_output_tokens":%d`, reasoning[0])
	}
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d%s,"total_tokens":%d}}}}`,
		ts, input, cached, output, r, input+output)
}

func TestCompact_CodexFixture(t *testing.T) {
	t.Parallel()

	codexOpts := agentOpts("codex")
	assertFixtureTransform(t, codexOpts, "testdata/codex_full.jsonl", "testdata/codex_expected.jsonl")
}

func TestCompact_CodexInlineCases(t *testing.T) {
	t.Parallel()

	codexOpts := agentOpts("codex")

	c1 := `{"type":"tool_use","id":"c1","name":"exec_command","input":{"cmd":"x"},"result":{"output":"ok","status":"success"}}`

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
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t3","content":[{"type":"text","text":"Running the command."},{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"ls"},"result":{"output":"file1.txt\nfile2.txt","status":"success"}}]}`,
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
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t3","content":[{"type":"text","text":"Creating the file now."},{"type":"tool_use","id":"call_1","name":"apply_patch","input":{"input":"*** Begin Patch\n*** Add File: hello.txt\n+Hello World\n*** End Patch\n"},"result":{"output":"Success. Updated: A hello.txt","status":"success"}}]}`,
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
			name: "tool output ends the model call without a usage event",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running then patching."}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}
{"timestamp":"t4","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file.txt"}}
{"timestamp":"t5","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"patch data"}}
{"timestamp":"t6","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":{"type":"text","text":"ok"}}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t3","content":[{"type":"text","text":"Running then patching."},{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"ls"},"result":{"output":"file.txt","status":"success"}}]}`,
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t5","content":[{"type":"tool_use","id":"call_2","name":"apply_patch","input":{"input":"patch data"},"result":{"output":"ok","status":"success"}}]}`,
			},
		},
		{
			name: "output of the other tool kind for the same call id attaches no result",
			input: []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}
{"timestamp":"t3","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":{"type":"text","text":"ignored"}}}
{"timestamp":"t4","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"patch data"}}
{"timestamp":"t5","type":"response_item","payload":{"type":"function_call_output","call_id":"call_2","output":"ignored"}}
`),
			expected: []string{
				`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t4","content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"ls"}},{"type":"tool_use","id":"call_2","name":"apply_patch","input":{"input":"patch data"}}]}`,
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
		{
			name:  "reasoning-only calls and usage with no open call keep their usage",
			input: cxJoin(cxUser("t1", "hi"), cxReasoning("t2"), cxTokens("t3", 10, 0, 4, 4), cxTokens("t4", 20, 2, 9)),
			expected: []string{
				cxPre + `"type":"user","ts":"t1","content":[{"text":"hi"}]}`,
				cxPre + `"type":"assistant","ts":"t3","input_tokens":10,"output_tokens":4,"reasoning_tokens":4,"content":[]}`,
				cxPre + `"type":"assistant","ts":"t4","input_tokens":8,"cache_read_tokens":2,"output_tokens":5,"content":[]}`,
			},
		},
		{
			name: "counter reset stops usage on later lines",
			input: cxJoin(cxUser("t01", "go"), cxModel("gpt-a"),
				cxMessage("t02", "one"), cxTokens("t03", 10, 0, 4, 2),
				cxMessage("t04", "two"), cxTokens("t05", 2, 0, 1, 0),
				cxMessage("t06", "three"), cxTokens("t07", 30, 0, 10, 8)),
			expected: []string{
				cxPre + `"type":"user","ts":"t01","content":[{"text":"go"}]}`,
				cxPre + `"type":"assistant","ts":"t03","model":"gpt-a","input_tokens":10,"output_tokens":4,"reasoning_tokens":2,"content":[{"type":"text","text":"one"}]}`,
				cxPre + `"type":"assistant","ts":"t05","content":[{"type":"text","text":"two"}]}`,
				cxPre + `"type":"assistant","ts":"t07","content":[{"type":"text","text":"three"}]}`,
			},
		},
		{
			name: "interleaved calls and outputs before one snapshot stay one call",
			input: cxJoin(cxUser("t1", "go"), cxCall("t2", "c1"), cxOutput("t3", "c1", "ok"),
				cxCall("t4", "c2"), cxOutput("t5", "c2", "ok"), cxTokens("t6", 10, 0, 4)),
			expected: []string{
				cxPre + `"type":"user","ts":"t1","content":[{"text":"go"}]}`,
				cxPre + `"type":"assistant","ts":"t6","input_tokens":10,"output_tokens":4,"content":[` + c1 + `,` + strings.Replace(c1, `"c1"`, `"c2"`, 1) + `]}`,
			},
		},
		{
			name: "a snapshot missing a counter neither closes a call nor moves the baseline",
			input: cxJoin(cxUser("t1", "go"), cxMessage("t2", "a"), cxTokens("t3", 10, 0, 4),
				cxMessage("t4", "b"),
				`{"timestamp":"t5","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"output_tokens":8}}}}`,
				`{"timestamp":"t6","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{}}}}`,
				cxTokens("t7", 20, 2, 9)),
			expected: []string{
				cxPre + `"type":"user","ts":"t1","content":[{"text":"go"}]}`,
				cxPre + `"type":"assistant","ts":"t3","input_tokens":10,"output_tokens":4,"content":[{"type":"text","text":"a"}]}`,
				cxPre + `"type":"assistant","ts":"t7","input_tokens":8,"cache_read_tokens":2,"output_tokens":5,"content":[{"type":"text","text":"b"}]}`,
			},
		},
		{
			name: "a repeated snapshot does not close an open call",
			input: cxJoin(cxUser("t1", "go"), cxMessage("t2", "a"), cxCall("t3", "c1"),
				cxTokens("t4", 0, 0, 0), cxOutput("t5", "c1", "ok"), cxTokens("t6", 10, 0, 4)),
			expected: []string{
				cxPre + `"type":"user","ts":"t1","content":[{"text":"go"}]}`,
				cxPre + `"type":"assistant","ts":"t6","input_tokens":10,"output_tokens":4,"content":[{"type":"text","text":"a"},` + c1 + `]}`,
			},
		},
		{
			name: "an output after its call closed lands on that call only",
			input: cxJoin(cxUser("t1", "go"), cxCall("t2", "c1"), cxTokens("t3", 10, 0, 4),
				cxOutput("t4", "c1", "ok"), cxMessage("t5", "next"), cxTokens("t6", 20, 0, 9)),
			expected: []string{
				cxPre + `"type":"user","ts":"t1","content":[{"text":"go"}]}`,
				cxPre + `"type":"assistant","ts":"t3","input_tokens":10,"output_tokens":4,"content":[` + c1 + `]}`,
				cxPre + `"type":"assistant","ts":"t6","input_tokens":10,"output_tokens":5,"content":[{"type":"text","text":"next"}]}`,
			},
		},
		{
			name: "unsupported usage shapes are ignored",
			input: cxJoin(cxUser("t1", "go"), cxMessage("t2", "hello"),
				`{"timestamp":"t3","type":"event_msg","payload":{"type":"token_count","input_tokens":500,"output_tokens":80}}`,
				`{"timestamp":"t4","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":9}}}}`),
			expected: []string{
				cxPre + `"type":"user","ts":"t1","content":[{"text":"go"}]}`,
				cxPre + `"type":"assistant","ts":"t2","content":[{"type":"text","text":"hello"}]}`,
			},
		},
		{
			name: "non-string snapshot timestamp keeps usage, the call boundary and the last item ts",
			input: cxJoin(cxMessage("t1", "one"), strings.Replace(cxTokens("x", 10, 0, 4), `"x"`, `123`, 1),
				cxMessage("t3", "two"), cxTokens("t4", 20, 0, 8)),
			expected: []string{
				cxPre + `"type":"assistant","ts":"t1","input_tokens":10,"output_tokens":4,"content":[{"type":"text","text":"one"}]}`,
				cxPre + `"type":"assistant","ts":"t4","input_tokens":10,"output_tokens":4,"content":[{"type":"text","text":"two"}]}`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := Convert(tc.input, codexOpts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertJSONLines(t, result, tc.expected)
		})
	}
}

func TestCodexOneLinePerCall_SumsEqualNativeDeltas(t *testing.T) {
	t.Parallel()

	// Real Codex order: tool outputs arrive before the token_count that closes
	// their call; parallel calls look like CALL CALL OUT OUT TC.
	native := cxJoin(
		cxUser("t01", "go"),
		cxReasoning("t02"),
		cxTokens("t03", 100, 40, 10, 5),
		cxModel("gpt-a"),
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"turn_context","payload":{"model":""}}`,
		cxReasoning("t04"),
		cxMessage("t05", "on it"),
		cxCall("t06", "c1"),
		cxCall("t07", "c2"),
		cxOutput("t08", "c1", "out1"),
		cxOutput("t09", "c2", "out2"),
		cxTokens("t10", 300, 140, 50, 15),
		cxTokens("t11", 300, 140, 50, 15),
		cxModel("gpt-b"),
		cxCall("t12", "c3"),
		cxOutput("t13", "c3", "out3"),
		cxTokens("t14", 600, 340, 90, 15),
		cxMessage("t15", "done"),
		cxTokens("t16", 700, 400, 120, 25),
	)
	tool := func(id string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"exec_command","input":{"cmd":"x"},"result":{"output":"out` + id[1:] + `","status":"success"}}`
	}

	out, err := Convert(native, agentOpts("codex"))
	if err != nil {
		t.Fatal(err)
	}
	assertJSONLines(t, out, []string{
		cxPre + `"type":"user","ts":"t01","content":[{"text":"go"}]}`,
		cxPre + `"type":"assistant","ts":"t03","input_tokens":60,"cache_read_tokens":40,"output_tokens":10,"reasoning_tokens":5,"content":[]}`,
		cxPre + `"type":"assistant","ts":"t10","model":"gpt-a","input_tokens":100,"cache_read_tokens":100,"output_tokens":40,"reasoning_tokens":10,"content":[{"type":"text","text":"on it"},` + tool("c1") + `,` + tool("c2") + `]}`,
		cxPre + `"type":"assistant","ts":"t14","model":"gpt-b","input_tokens":100,"cache_read_tokens":200,"output_tokens":40,"content":[` + tool("c3") + `]}`,
		cxPre + `"type":"assistant","ts":"t16","model":"gpt-b","input_tokens":40,"cache_read_tokens":60,"output_tokens":30,"reasoning_tokens":10,"content":[{"type":"text","text":"done"}]}`,
	})

	var sum [4]int64 // input+cache read, cache read, output, reasoning
	for _, raw := range nonEmptyLines(out) {
		var l struct {
			In   int64 `json:"input_tokens"`
			Read int64 `json:"cache_read_tokens"`
			Out  int64 `json:"output_tokens"`
			Rsn  int64 `json:"reasoning_tokens"`
		}
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatal(err)
		}
		sum = [4]int64{sum[0] + l.In + l.Read, sum[1] + l.Read, sum[2] + l.Out, sum[3] + l.Rsn}
	}
	if want := [4]int64{700, 400, 120, 25}; sum != want {
		t.Errorf("per-line usage sums to %v, want native totals %v", sum, want)
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
	opts := Options{Agent: "codex", CLIVersion: "0.5.1", StartLine: 4}

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

	result, err := Convert(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_CodexStartLine_SeedsUsageFromSkippedPrefix(t *testing.T) {
	t.Parallel()

	// The cumulative counter keeps running across the skipped prefix, so the
	// first snapshot in the slice must be a delta from the prefix's last one.
	opts := Options{Agent: "codex", CLIVersion: "0.5.1", StartLine: 4}

	input := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}}
{"timestamp":"t4","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":30}}}}
{"timestamp":"t5","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second prompt"}]}}
{"timestamp":"t6","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second answer"}]}}
{"timestamp":"t7","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":50,"output_tokens":45}}}}
`)

	expected := []string{
		`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"user","ts":"t5","content":[{"text":"second prompt"}]}`,
		`{"v":1,"agent":"codex","cli_version":"0.5.1","type":"assistant","ts":"t7","input_tokens":20,"output_tokens":15,"cache_read_tokens":30,"content":[{"type":"text","text":"second answer"}]}`,
	}

	result, err := Convert(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_CodexStartLine_IncludesTheLastCallTheSliceChanges(t *testing.T) {
	t.Parallel()

	tool := func(id string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"exec_command","input":{"cmd":"x"},"result":{"output":"ok","status":"success"}}`
	}
	tests := []struct {
		name      string
		input     []byte
		startLine int
		want      []string
	}{
		{
			name: "outputs and closing snapshot for the open call",
			input: cxJoin(cxUser("t1", "go"), cxCall("t2", "c1"), cxCall("t3", "c2"), cxOutput("t4", "c1", "ok"),
				cxOutput("t5", "c2", "ok"), cxTokens("t6", 10, 0, 4)),
			startLine: 4,
			want: []string{
				cxPre + `"type":"assistant","ts":"t6","input_tokens":10,"output_tokens":4,"content":[` + tool("c1") + `,` + tool("c2") + `]}`,
			},
		},
		{
			name:      "closing snapshot for the open call",
			input:     cxJoin(cxUser("t1", "go"), cxCall("t2", "c1"), cxOutput("t3", "c1", "ok"), cxTokens("t4", 10, 0, 4)),
			startLine: 3,
			want: []string{
				cxPre + `"type":"assistant","ts":"t4","input_tokens":10,"output_tokens":4,"content":[` + tool("c1") + `]}`,
			},
		},
		{
			name:      "late output for the last call, closed at the boundary",
			input:     cxJoin(cxUser("t1", "go"), cxCall("t2", "c1"), cxTokens("t3", 10, 0, 4), cxOutput("t4", "c1", "ok")),
			startLine: 3,
			want: []string{
				cxPre + `"type":"assistant","ts":"t3","input_tokens":10,"output_tokens":4,"content":[` + tool("c1") + `]}`,
			},
		},
		{
			name: "calls closed before the boundary are not repeated",
			input: cxJoin(cxUser("t1", "go"), cxCall("t2", "c1"), cxTokens("t3", 10, 0, 4),
				cxUser("t4", "next"), cxMessage("t5", "b"), cxTokens("t6", 20, 0, 8),
				cxOutput("t7", "c1", "ok")),
			startLine: 6,
			want:      nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{Agent: "codex", CLIVersion: "0.5.1", StartLine: tc.startLine}

			delta, err := Convert(tc.input, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONLines(t, delta, tc.want)

			full, boundary, err := ConvertWithBoundary(tc.input, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONLines(t, []byte(strings.Join(nonEmptyLines(full)[boundary:], "\n")+"\n"), tc.want)
		})
	}
}
