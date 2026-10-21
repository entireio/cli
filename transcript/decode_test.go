package transcript

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecode_Blocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  Line
	}{
		{
			name:  "user block without type is normalized to text",
			input: `{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"id":"b1","text":"hello"}]}`,
			want: Line{
				Version: 1, Type: TypeUser, Agent: "claude-code", CLIVersion: "0.5.1", Timestamp: "2026-01-01T00:00:00Z",
				Blocks: []Block{{Type: BlockText, ID: "b1", Text: "hello", Raw: json.RawMessage(`{"id":"b1","text":"hello"}`)}},
			},
		},
		{
			name:  "assistant text with ids and tokens",
			input: `{"v":1,"type":"assistant","id":"msg-1","input_tokens":100,"output_tokens":50,"content":[{"type":"text","text":"Hi!"}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant, ID: "msg-1", InputTokens: 100, OutputTokens: 50,
				Blocks: []Block{{Type: BlockText, Text: "Hi!", Raw: json.RawMessage(`{"type":"text","text":"Hi!"}`)}},
			},
		},
		{
			name:  "tool_use with full result",
			input: `{"v":1,"type":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/a"},"result":{"output":"ok","status":"success","file":{"filePath":"/a","numLines":3},"matchCount":2}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{
					Type: BlockToolUse, ID: "tu-1", Name: "Read", Input: json.RawMessage(`{"file_path":"/a"}`),
					Result: &ToolResult{Output: "ok", Status: ToolStatusSuccess, File: &ToolResultFile{FilePath: "/a", NumLines: 3}, MatchCount: 2},
					Raw:    json.RawMessage(`{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/a"},"result":{"output":"ok","status":"success","file":{"filePath":"/a","numLines":3},"matchCount":2}}`),
				}},
			},
		},
		{
			name:  "tool_use with error result and no file",
			input: `{"v":1,"type":"assistant","content":[{"type":"tool_use","name":"Bash","input":{},"result":{"output":"boom","status":"error"}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{
					Type: BlockToolUse, Name: "Bash", Input: json.RawMessage(`{}`),
					Result: &ToolResult{Output: "boom", Status: ToolStatusError},
					Raw:    json.RawMessage(`{"type":"tool_use","name":"Bash","input":{},"result":{"output":"boom","status":"error"}}`),
				}},
			},
		},
		{
			name:  "image passes through with raw",
			input: `{"v":1,"type":"assistant","content":[{"type":"image","source":{"type":"base64","data":"abc"}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{Type: BlockImage, Raw: json.RawMessage(`{"type":"image","source":{"type":"base64","data":"abc"}}`)}},
			},
		},
		{
			name:  "unknown block type keeps type and raw",
			input: `{"v":1,"type":"assistant","content":[{"type":"widget","x":1}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{Type: "widget", Raw: json.RawMessage(`{"type":"widget","x":1}`)}},
			},
		},
		{
			name:  "non-object block keeps only raw",
			input: `{"v":1,"type":"user","content":["str",7]}`,
			want: Line{
				Version: 1, Type: TypeUser,
				Blocks: []Block{{Raw: json.RawMessage(`"str"`)}, {Raw: json.RawMessage(`7`)}},
			},
		},
		{
			name:  "block with wrong-typed known field keeps only raw",
			input: `{"v":1,"type":"user","content":[{"type":"text","text":5}]}`,
			want: Line{
				Version: 1, Type: TypeUser,
				Blocks: []Block{{Raw: json.RawMessage(`{"type":"text","text":5}`)}},
			},
		},
		{
			name:  "string content becomes one text block",
			input: `{"v":1,"type":"assistant","content":"hello"}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{Type: BlockText, Text: "hello", Raw: json.RawMessage(`{"type":"text","text":"hello"}`)}},
			},
		},
		{
			name:  "string content raw is not HTML-escaped",
			input: `{"v":1,"type":"assistant","content":"a<b && c"}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{Type: BlockText, Text: "a<b && c", Raw: json.RawMessage(`{"type":"text","text":"a<b && c"}`)}},
			},
		},
		{
			name:  "null content has no blocks",
			input: `{"v":1,"type":"assistant","content":null}`,
			want:  Line{Version: 1, Type: TypeAssistant},
		},
		{
			name:  "absent content has no blocks",
			input: `{"v":1,"type":"assistant"}`,
			want:  Line{Version: 1, Type: TypeAssistant},
		},
		{
			name:  "content of another JSON type has no blocks",
			input: `{"v":1,"type":"assistant","content":{"a":1}}`,
			want:  Line{Version: 1, Type: TypeAssistant},
		},
		{
			name:  "non-string ts is tolerated and ignored",
			input: `{"v":1,"type":"user","ts":1712345678000}`,
			want:  Line{Version: 1, Type: TypeUser},
		},
		{
			name:  "unknown fields are ignored",
			input: `{"v":1,"type":"user","future":{"x":1},"content":[{"text":"a","extra":true}]}`,
			want: Line{
				Version: 1, Type: TypeUser,
				Blocks: []Block{{Type: BlockText, Text: "a", Raw: json.RawMessage(`{"text":"a","extra":true}`)}},
			},
		},
		{
			name:  "unknown line type is returned as is",
			input: `{"v":1,"type":"system","content":[]}`,
			want:  Line{Version: 1, Type: "system", Blocks: []Block{}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, err := Decode([]byte(tc.input + "\n"))
			require.NoError(t, err)
			require.Len(t, lines, 1)
			assert.Equal(t, tc.want, lines[0])
		})
	}
}

func TestDecode_SkippedLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		reason string
	}{
		{"malformed JSON", `{"v":1,"type":`, "not a JSON object"},
		{"array", `[1,2]`, "not a JSON object"},
		{"string", `"hello"`, "not a JSON object"},
		{"missing v", `{"type":"user","content":[]}`, "missing v"},
		{"unsupported v", `{"v":2,"type":"user","content":[]}`, "unsupported v: 2"},
		{"missing type", `{"v":1,"content":[]}`, "missing type"},
		{"empty type", `{"v":1,"type":"","content":[]}`, "missing type"},
		{"wrong-typed agent", `{"v":1,"type":"user","agent":42}`, "invalid field: agent"},
		{"wrong-typed tokens", `{"v":1,"type":"assistant","input_tokens":"x"}`, "invalid field: input_tokens"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, err := Decode([]byte(tc.input + "\n"))
			assert.Empty(t, lines)
			var skipped *SkippedLinesError
			require.ErrorAs(t, err, &skipped)
			assert.Equal(t, []SkippedLine{{Line: 1, Reason: tc.reason}}, skipped.Skipped)
		})
	}
}

func TestDecode_PartialResultWithError(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"v":1,"type":"user","content":[{"text":"a"}]}`,
		``,
		`   `,
		`not json`,
		`{"v":1,"type":"assistant","content":"b"}`,
		`{"v":3,"type":"user"}`,
	}, "\n")

	lines, err := Decode([]byte(input))

	require.Len(t, lines, 2)
	assert.Equal(t, TypeUser, lines[0].Type)
	assert.Equal(t, "b", lines[1].Blocks[0].Text)

	var skipped *SkippedLinesError
	require.ErrorAs(t, err, &skipped)
	assert.Equal(t, []SkippedLine{
		{Line: 4, Reason: "not a JSON object"},
		{Line: 6, Reason: "unsupported v: 3"},
	}, skipped.Skipped)
	assert.Contains(t, err.Error(), "skipped 2 line(s)")
	assert.Contains(t, err.Error(), "line 4: not a JSON object")
}

func TestDecode_EmptyInput(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "\n\n", "  \n\t\n"} {
		lines, err := Decode([]byte(in))
		require.NoError(t, err)
		assert.Nil(t, lines)
	}
}

func TestDecode_CRLF(t *testing.T) {
	t.Parallel()

	lines, err := Decode([]byte("{\"v\":1,\"type\":\"user\",\"content\":[{\"text\":\"a\"}]}\r\n{\"v\":1,\"type\":\"user\",\"content\":[{\"text\":\"b\"}]}\r\n"))
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "b", lines[1].Blocks[0].Text)
}

func TestDecode_VeryLongLine(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("x", 11*1024*1024)
	lines, err := Decode([]byte(`{"v":1,"type":"user","content":[{"text":"` + text + `"}]}` + "\n"))
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Len(t, lines[0].Blocks[0].Text, len(text))
}

func TestSkippedLinesError_Empty(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "transcript: skipped 0 lines", (&SkippedLinesError{}).Error())
}
