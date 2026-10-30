package transcript

import (
	"bytes"
	"strings"
	"testing"
)

// toolResults decodes compact output and maps each tool_use ID to its
// result.
func toolResults(t *testing.T, compact []byte) map[string]*ToolResult {
	t.Helper()
	lines, err := Decode(compact)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	results := make(map[string]*ToolResult)
	for _, l := range lines {
		for _, b := range l.Blocks {
			if b.Type == BlockToolUse {
				results[b.ID] = b.Result
			}
		}
	}
	return results
}

func convertResults(t *testing.T, input string, agent string) map[string]*ToolResult {
	t.Helper()
	out, err := Convert([]byte(input), agentOpts(agent))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	return toolResults(t, out)
}

func TestJSONLAdapter_AttachesResultsByToolUseID(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"parallel results in separate user entries", "result after the next assistant message began", "results by ID attach after a prompt", "results by ID attach after injected text", "results without an ID only until the next prompt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := jsonlEdgeCases[name]
			results := convertResults(t, input, "claude-code")
			want := map[string]string{
				"parallel results in separate user entries":        "tu1=first tu2=second",
				"result after the next assistant message began":    "tu1=late tu2=no id: last message",
				"results by ID attach after a prompt":              "tu1=later",
				"results by ID attach after injected text":         "tu1=loaded tu2=late",
				"results without an ID only until the next prompt": "tu1=-",
			}[name]
			var got []string
			for _, id := range []string{"tu1", "tu2"} {
				r, ok := results[id]
				switch {
				case !ok:
					continue
				case r == nil:
					got = append(got, id+"=-")
				default:
					got = append(got, id+"="+r.Output)
				}
			}
			if strings.Join(got, " ") != want {
				t.Errorf("results = %q, want %q", strings.Join(got, " "), want)
			}

			// Fed one line at a time, the records still fold to Convert's output.
			convert, err := Convert([]byte(input), defaultOpts)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.SplitAfter([]byte(input), []byte("\n"))
			if fold := foldRecords(t, adapterRecords([]byte(input), nil, lines)); !bytes.Equal(fold, convert) {
				t.Errorf("line-by-line fold differs from Convert\ngot:\n%s\nwant:\n%s", fold, convert)
			}
		})
	}
}

func TestToolResultAgent_PerConverter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, agent, input, toolUseID, want string
	}{
		{
			name:  "claude Agent call",
			agent: "claude-code",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"subagent_type":"general-purpose"}}]}}
{"type":"user","timestamp":"t2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"a17832fedf789f647","resolvedModel":"claude-opus-5-5"}}
`,
			toolUseID: "toolu_1",
			want:      "a17832fedf789f647",
		},
		{
			name:  "claude forked Skill call",
			agent: "claude-code",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_2","name":"Skill","input":{"skill":"review"}}]}}
{"type":"user","timestamp":"t2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"forked"}]},"toolUseResult":{"success":true,"commandName":"review","status":"forked","agentId":"a2b3c4d5e6f7a8b9c"}}
`,
			toolUseID: "toolu_2",
			want:      "a2b3c4d5e6f7a8b9c",
		},
		{
			name:  "codex spawn_agent",
			agent: "codex",
			input: `{"timestamp":"t0","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t1","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Spawning."}]}}
{"timestamp":"t2","type":"response_item","payload":{"type":"function_call","name":"spawn_agent","arguments":"{\"task_name\":\"alpha\"}","call_id":"call_1"}}
{"timestamp":"t3","type":"event_msg","payload":{"type":"item_completed","item":{"type":"SubAgentActivity","model":"gpt-6","id":"call_1","kind":"started","agent_thread_id":"thread-alpha","agent_path":"/root/alpha"}}}
{"timestamp":"t4","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"started"}}
{"timestamp":"t5","type":"event_msg","payload":{"type":"item_completed","item":{"type":"SubAgentActivity","id":"subagent-completed-1","kind":"completed","agent_thread_id":"thread-alpha"}}}
`,
			toolUseID: "call_1",
			want:      "thread-alpha",
		},
		{
			name:      "opencode task",
			agent:     "opencode",
			input:     `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"a1","role":"assistant","time":{"created":1000}},"parts":[{"type":"tool","tool":"task","callID":"call_1","state":{"status":"completed","input":{"subagent_type":"explore"},"output":"done","metadata":{"sessionId":"ses_child"}}}]}]}`,
			toolUseID: "call_1",
			want:      "ses_child",
		},
		{
			name:      "opencode non-task tool with a sessionId",
			agent:     "opencode",
			input:     `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"a1","role":"assistant","time":{"created":1000}},"parts":[{"type":"tool","tool":"bash","callID":"call_2","state":{"status":"completed","input":{},"output":"ok","metadata":{"sessionId":"ses_other"}}}]}]}`,
			toolUseID: "call_2",
		},
		{
			name:  "copilot task",
			agent: "copilot-cli",
			input: `{"type":"session.start","data":{"sessionId":"sess-1"},"timestamp":"t0"}
{"type":"assistant.message","data":{"messageId":"m1","content":"","toolRequests":[{"toolCallId":"call_1","name":"task","arguments":{}}]},"timestamp":"t1"}
{"type":"subagent.started","agentId":"child-1","data":{"toolCallId":"call_1","agentName":"general-purpose"},"timestamp":"t2"}
{"type":"tool.execution_complete","data":{"toolCallId":"call_1","success":true,"result":{"content":"done"}},"timestamp":"t3"}
`,
			toolUseID: "call_1",
			want:      "child-1",
		},
		{
			name:  "a call that launched nothing",
			agent: "claude-code",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_3","name":"Bash","input":{}}]}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"ok"}]},"toolUseResult":{"stdout":"ok"}}
`,
			toolUseID: "toolu_3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := convertResults(t, tc.input, tc.agent)[tc.toolUseID]
			if r == nil {
				t.Fatalf("tool_use %s has no result", tc.toolUseID)
			}
			got := ""
			if r.Agent != nil {
				got = r.Agent.ID
			}
			if got != tc.want {
				t.Errorf("agent id = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClaudeToolResultContent covers a tool_result whose content is an array
// of blocks, as MCP tools write it: the text blocks' text is the output,
// joined line by line, and image bytes never reach it. Checkpoints (Convert)
// and live records (NewClaudeAdapter) must agree.
func TestClaudeToolResultContent(t *testing.T) {
	t.Parallel()

	const imageBlock = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg"}}`
	tests := []struct {
		name, content, want string
	}{
		{name: "string content", content: `"line one\nline two"`, want: "line one\nline two"},
		{name: "text blocks", content: `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`, want: "first\nsecond"},
		{name: "text and image", content: `[{"type":"text","text":"took a screenshot"},` + imageBlock + `,{"type":"text","text":"done"}]`, want: "took a screenshot\ndone"},
		{name: "image only", content: `[` + imageBlock + `]`, want: ""},
		{name: "empty array", content: `[]`, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__browser__screenshot","input":{}}]}}
{"type":"user","timestamp":"t2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + tc.content + `}]}}
`
			stored := convertResults(t, input, "claude-code")["toolu_1"]
			if stored == nil {
				t.Fatal("stored: tool_use toolu_1 has no result")
			}
			if stored.Output != tc.want {
				t.Errorf("stored output = %q, want %q", stored.Output, tc.want)
			}

			a := NewClaudeAdapter()
			records := append(a.Feed([]byte(input)), a.Flush()...)
			var live *ToolResult
			for _, r := range records {
				if r.Op == OpResult && r.ToolUseID == "toolu_1" {
					live = r.Result
				}
			}
			if live == nil {
				t.Fatal("live: no result record for toolu_1")
			}
			if live.Output != tc.want {
				t.Errorf("live output = %q, want %q", live.Output, tc.want)
			}
		})
	}
}
