package transcript

import (
	"bytes"
	"testing"

	"github.com/entireio/cli/internal/transcript/opencodev2"
)

func TestCompact_OpenCodeFixture(t *testing.T) {
	t.Parallel()
	assertFixtureTransform(t, agentOpts("opencode"), "testdata/opencode_full.jsonl", "testdata/opencode_expected.jsonl")
}

func TestCompact_OpenCodeTokenUsage(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"info":{"id":"ses-1","title":"test","version":"1.0"},
		"messages":[
			{
				"info":{"id":"msg-u1","role":"user","time":{"created":1700000000000}},
				"parts":[{"type":"text","text":"hello"}]
			},
			{
				"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000,"completed":1700000002000},
					"tokens":{"total":500,"input":150,"output":90,"reasoning":0,"cache":{"read":100,"write":160}}},
				"parts":[{"type":"text","text":"Hi there!"}]
			}
		]
	}`)

	expected := []string{
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"user","ts":"2023-11-14T22:13:20Z","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:21Z","id":"msg-a1","input_tokens":150,"output_tokens":90,"content":[{"type":"text","text":"Hi there!"}]}`,
	}

	result, err := Convert(input, agentOpts("opencode"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_OpenCodeStartLine(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"info":{"id":"ses-1","title":"test","version":"1.0"},
		"messages":[
			{
				"info":{"id":"msg-u1","role":"user","time":{"created":1700000000000}},
				"parts":[{"type":"text","text":"hello"}]
			},
			{
				"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000}},
				"parts":[{"type":"text","text":"Hi there!"}]
			},
			{
				"info":{"id":"msg-u2","role":"user","time":{"created":1700000002000}},
				"parts":[{"type":"text","text":"bye"}]
			}
		]
	}`)

	t.Run("skip first message", func(t *testing.T) {
		t.Parallel()
		opts := Options{Agent: "opencode", CLIVersion: "0.5.1", StartLine: 1}
		expected := []string{
			`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:21Z","id":"msg-a1","content":[{"type":"text","text":"Hi there!"}]}`,
			`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"user","ts":"2023-11-14T22:13:22Z","content":[{"text":"bye"}]}`,
		}
		result, err := Convert(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertJSONLines(t, result, expected)
	})

	t.Run("skip all messages", func(t *testing.T) {
		t.Parallel()
		opts := Options{Agent: "opencode", CLIVersion: "0.5.1", StartLine: 100}
		result, err := Convert(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertJSONLines(t, result, nil)
	})
}

func TestCompact_OpenCodeNoTokensOmitsFields(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"info":{"id":"ses-1","title":"test","version":"1.0"},
		"messages":[
			{
				"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000}},
				"parts":[{"type":"text","text":"no tokens here"}]
			}
		]
	}`)

	expected := []string{
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:21Z","id":"msg-a1","content":[{"type":"text","text":"no tokens here"}]}`,
	}

	result, err := Convert(input, agentOpts("opencode"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// An assistant message with nothing to show has no line, unless it has
// tokens: then it keeps a line with no content, so its usage is counted. A
// tool that is still pending or running has no result yet.
func TestCompact_OpenCodeEmptyAssistantAndRunningTool(t *testing.T) {
	t.Parallel()

	input := []byte(`{"info":{"id":"ses-1"},"messages":[
		{"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000}},
		 "parts":[{"type":"step-start"}]},
		{"info":{"id":"msg-a2","role":"assistant","time":{"created":1700000002000},"tokens":{"input":7,"output":3}},
		 "parts":[{"type":"reasoning","text":"hmm"}]},
		{"info":{"id":"msg-a3","role":"assistant","time":{"created":1700000003000}},
		 "parts":[{"type":"tool","callID":"c1","tool":"bash","state":{"status":"running","input":{"command":"ls"}}},
		          {"type":"tool","callID":"c2","tool":"bash","state":{"status":"pending","input":{}}}]}
	]}`)

	expected := []string{
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:22Z","id":"msg-a2","input_tokens":7,"output_tokens":3,"content":null}`,
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:23Z","id":"msg-a3","content":[{"id":"c1","input":{"command":"ls"},"name":"bash","type":"tool_use"},{"id":"c2","input":{},"name":"bash","type":"tool_use"}]}`,
	}

	result, err := Convert(input, agentOpts("opencode"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

// Convert reads an OpenCode 2 export as its normalized form.
func TestCompact_OpenCodeV2Export(t *testing.T) {
	t.Parallel()

	raw := []byte(openCodeV2Steps[len(openCodeV2Steps)-1])
	normalized, err := opencodev2.Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(normalized, raw) {
		t.Fatal("fixture is not an OpenCode 2 export")
	}
	got, err := Convert(raw, agentOpts("opencode"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := Convert(normalized, agentOpts("opencode"))
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || !bytes.Equal(got, want) {
		t.Fatalf("Convert of the 2.x export:\n%s\nwant (normalized):\n%s", got, want)
	}
}

// A failed tool keeps OpenCode's error message: 1.x records it in the state's
// error, with no output, and the 2.x conversion carries it there too.
func TestCompact_OpenCodeFailedToolKeepsError(t *testing.T) {
	t.Parallel()

	v1 := []byte(`{"info":{"id":"ses-1"},"messages":[
		{"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000}},
		 "parts":[{"type":"tool","callID":"c1","tool":"read","state":{"status":"error","input":{"filePath":"/repo/missing.go"},"error":"File not found: /repo/missing.go","time":{"start":1700000001100,"end":1700000001200}}}]}
	]}`)
	v2 := []byte(`{"info":{"id":"ses_2","location":{"directory":"/repo"}},"messages":[
	 {"id":"msg_u1","type":"user","text":"Read it.","files":[],"time":{"created":1700000000000}},
	 {"id":"msg_a1","type":"assistant","agent":"build","model":{"id":"mock","providerID":"mock"},"time":{"created":1700000001000,"completed":1700000002000},
	  "content":[{"type":"tool","id":"c1","name":"read","state":{"status":"error","input":{"path":"missing.go"},"content":[],"structured":{},"error":{"type":"unknown","message":"File not found: /repo/missing.go"}},"time":{"created":1700000001100,"ran":1700000001200,"completed":1700000001300}}]}
	]}`)
	for name, input := range map[string][]byte{"1.x": v1, "2.x": v2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := Convert(input, agentOpts("opencode"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := []byte(`"result":{"output":"File not found: /repo/missing.go","status":"error"}`)
			if !bytes.Contains(result, want) {
				t.Fatalf("failed tool's result lost its error message:\n%s", result)
			}
		})
	}
}
