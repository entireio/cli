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

	fiveMinute := int64(3000000003)
	oneHour := int64(3000000004)
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
			name:  "assistant per-call model and usage with unknown fields",
			input: `{"v":1,"type":"assistant","model":"native-model","input_tokens":100,"output_tokens":50,"cache_read_tokens":3000000001,"cache_write_tokens":3000000002,"cache_write_5m_tokens":3000000003,"cache_write_1h_tokens":3000000004,"reasoning_tokens":3000000005,"future":{"x":1}}`,
			want: Line{
				Version: 1, Type: TypeAssistant, Model: "native-model", InputTokens: 100, OutputTokens: 50,
				CacheReadTokens: 3000000001, CacheWriteTokens: 3000000002,
				CacheWrite5mTokens: &fiveMinute, CacheWrite1hTokens: &oneHour, ReasoningTokens: 3000000005,
			},
		},
		{
			name:  "old minimal assistant line",
			input: `{"v":1,"type":"assistant"}`,
			want:  Line{Version: 1, Type: TypeAssistant},
		},
		{
			name:  "historical cache-inclusive input is not reinterpreted",
			input: `{"v":1,"agent":"codex","type":"assistant","input_tokens":100,"cache_read_tokens":80}`,
			want:  Line{Version: 1, Type: TypeAssistant, Agent: "codex", InputTokens: 100, CacheReadTokens: 80},
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
			name:  "tool_use result naming its subagent",
			input: `{"v":1,"type":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Agent","input":{},"result":{"output":"","status":"success","agent":{"id":"a17"}}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{
					Type: BlockToolUse, ID: "tu-1", Name: "Agent", Input: json.RawMessage(`{}`),
					Result: &ToolResult{Status: ToolStatusSuccess, Agent: &ToolResultAgent{ID: "a17"}},
					Raw:    json.RawMessage(`{"type":"tool_use","id":"tu-1","name":"Agent","input":{},"result":{"output":"","status":"success","agent":{"id":"a17"}}}`),
				}},
			},
		},
		{
			name:  "tool_use result with a malformed agent keeps the block",
			input: `{"v":1,"type":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Agent","input":{},"result":{"output":"x","status":"success","agent":"a17"}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{
					Type: BlockToolUse, ID: "tu-1", Name: "Agent", Input: json.RawMessage(`{}`),
					Result: &ToolResult{Output: "x", Status: ToolStatusSuccess},
					Raw:    json.RawMessage(`{"type":"tool_use","id":"tu-1","name":"Agent","input":{},"result":{"output":"x","status":"success","agent":"a17"}}`),
				}},
			},
		},
		{
			name:  "tool_use result with images",
			input: `{"v":1,"type":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{},"result":{"output":"","status":"success","images":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"entire-asset:assets/img-ab.png"}}]}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{
					Type: BlockToolUse, ID: "tu-1", Name: "Read", Input: json.RawMessage(`{}`),
					Result: &ToolResult{Status: ToolStatusSuccess, Images: []json.RawMessage{
						json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"entire-asset:assets/img-ab.png"}}`),
					}},
					Raw: json.RawMessage(`{"type":"tool_use","id":"tu-1","name":"Read","input":{},"result":{"output":"","status":"success","images":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"entire-asset:assets/img-ab.png"}}]}}`),
				}},
			},
		},
		{
			name:  "tool_use result with malformed images keeps the block",
			input: `{"v":1,"type":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{},"result":{"output":"x","status":"success","images":"img"}}]}`,
			want: Line{
				Version: 1, Type: TypeAssistant,
				Blocks: []Block{{
					Type: BlockToolUse, ID: "tu-1", Name: "Read", Input: json.RawMessage(`{}`),
					Result: &ToolResult{Output: "x", Status: ToolStatusSuccess},
					Raw:    json.RawMessage(`{"type":"tool_use","id":"tu-1","name":"Read","input":{},"result":{"output":"x","status":"success","images":"img"}}`),
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
		{"wrong-typed model", `{"v":1,"type":"assistant","model":42}`, "invalid field: model"},
		{"wrong-typed cache read", `{"v":1,"type":"assistant","cache_read_tokens":"x"}`, "invalid field: cache_read_tokens"},
		{"wrong-typed cache write", `{"v":1,"type":"assistant","cache_write_tokens":true}`, "invalid field: cache_write_tokens"},
		{"wrong-typed cache write 5m", `{"v":1,"type":"assistant","cache_write_5m_tokens":{}}`, "invalid field: cache_write_5m_tokens"},
		{"wrong-typed cache write 1h", `{"v":1,"type":"assistant","cache_write_1h_tokens":[]}`, "invalid field: cache_write_1h_tokens"},
		{"wrong-typed reasoning", `{"v":1,"type":"assistant","reasoning_tokens":1.5}`, "invalid field: reasoning_tokens"},
		{"overflowing input", `{"v":1,"type":"assistant","input_tokens":9223372036854775808}`, "invalid field: input_tokens"},
		{"overflowing output", `{"v":1,"type":"assistant","output_tokens":9223372036854775808}`, "invalid field: output_tokens"},
		{"overflowing cache read", `{"v":1,"type":"assistant","cache_read_tokens":9223372036854775808}`, "invalid field: cache_read_tokens"},
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
		`{"v":1,"type":"assistant","content":"b","model":"native-model","cache_read_tokens":25}`,
		`{"v":3,"type":"user"}`,
		`{"v":1,"type":"assistant","reasoning_tokens":"x"}`,
		`{"v":1,"type":"assistant","reasoning_tokens":7}`,
	}, "\n")

	lines, err := Decode([]byte(input))

	require.Len(t, lines, 3)
	assert.Equal(t, TypeUser, lines[0].Type)
	assert.Equal(t, "b", lines[1].Blocks[0].Text)
	assert.Equal(t, "native-model", lines[1].Model)
	assert.Equal(t, int64(25), lines[1].CacheReadTokens)
	assert.Equal(t, Line{Version: 1, Type: TypeAssistant, ReasoningTokens: 7}, lines[2])

	var skipped *SkippedLinesError
	require.ErrorAs(t, err, &skipped)
	assert.Equal(t, []SkippedLine{
		{Line: 4, Reason: "not a JSON object"},
		{Line: 6, Reason: "unsupported v: 3"},
		{Line: 7, Reason: "invalid field: reasoning_tokens"},
	}, skipped.Skipped)
	assert.Contains(t, err.Error(), "skipped 3 line(s)")
	assert.Contains(t, err.Error(), "line 4: not a JSON object")
}

func TestDecode_NullOptionalUsageFields(t *testing.T) {
	t.Parallel()

	for _, field := range []string{
		"model",
		"input_tokens",
		"output_tokens",
		"cache_read_tokens",
		"cache_write_tokens",
		"cache_write_5m_tokens",
		"cache_write_1h_tokens",
		"reasoning_tokens",
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()

			lines, err := Decode([]byte(`{"v":1,"type":"assistant","` + field + `":null}`))
			require.NoError(t, err)
			assert.Equal(t, []Line{{Version: 1, Type: TypeAssistant}}, lines)
		})
	}
}

func TestDecode_CacheWriteSplitPresence(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	tests := []struct {
		name   string
		input  string
		want5m *int64
		want1h *int64
	}{
		{
			name:  "absent splits remain unknown",
			input: `{"v":1,"type":"assistant"}`,
		},
		{
			name:   "reported zero splits remain known",
			input:  `{"v":1,"type":"assistant","cache_write_5m_tokens":0,"cache_write_1h_tokens":0}`,
			want5m: &zero,
			want1h: &zero,
		},
		{
			name:   "only 5m is reported",
			input:  `{"v":1,"type":"assistant","cache_write_5m_tokens":0}`,
			want5m: &zero,
		},
		{
			name:   "only 1h is reported",
			input:  `{"v":1,"type":"assistant","cache_write_1h_tokens":0}`,
			want1h: &zero,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lines, err := Decode([]byte(tc.input))
			require.NoError(t, err)
			require.Len(t, lines, 1)
			assert.Equal(t, tc.want5m, lines[0].CacheWrite5mTokens)
			assert.Equal(t, tc.want1h, lines[0].CacheWrite1hTokens)
		})
	}
}

func TestDecode_OptionalUsageRoundTrip(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	fiveMinute := int64(3)
	oneHour := int64(7)
	tests := []struct {
		name string
		line transcriptLine
		json string
		want Line
	}{
		{
			name: "unreported usage is omitted",
			line: transcriptLine{V: 1, Type: TypeAssistant},
			json: `{"v":1,"agent":"","cli_version":"","type":"assistant","content":null}`,
			want: Line{Version: 1, Type: TypeAssistant},
		},
		{
			name: "reported cache split zeros are retained",
			line: transcriptLine{V: 1, Type: TypeAssistant, CacheWrite5mTokens: &zero, CacheWrite1hTokens: &zero},
			json: `{"v":1,"agent":"","cli_version":"","type":"assistant","cache_write_5m_tokens":0,"cache_write_1h_tokens":0,"content":null}`,
			want: Line{Version: 1, Type: TypeAssistant, CacheWrite5mTokens: &zero, CacheWrite1hTokens: &zero},
		},
		{
			name: "reported model and usage are retained",
			line: transcriptLine{
				V: 1, Type: TypeAssistant, Model: "native-model", InputTokens: 20, OutputTokens: 15,
				CacheReadTokens: 30, CacheWriteTokens: 10, CacheWrite5mTokens: &fiveMinute, CacheWrite1hTokens: &oneHour, ReasoningTokens: 5,
			},
			json: `{"v":1,"agent":"","cli_version":"","type":"assistant","model":"native-model","input_tokens":20,"output_tokens":15,"cache_read_tokens":30,"cache_write_tokens":10,"cache_write_5m_tokens":3,"cache_write_1h_tokens":7,"reasoning_tokens":5,"content":null}`,
			want: Line{
				Version: 1, Type: TypeAssistant, Model: "native-model", InputTokens: 20, OutputTokens: 15,
				CacheReadTokens: 30, CacheWriteTokens: 10, CacheWrite5mTokens: &fiveMinute, CacheWrite1hTokens: &oneHour, ReasoningTokens: 5,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal(tc.line)
			require.NoError(t, err)
			assert.JSONEq(t, tc.json, string(encoded))

			lines, err := Decode(encoded)
			require.NoError(t, err)
			assert.Equal(t, []Line{tc.want}, lines)
		})
	}
}

func TestDecode_Int64TokenCounts(t *testing.T) {
	t.Parallel()

	input, output := int64(3000000001), int64(3000000002)
	version, lineType := 1, TypeAssistant
	tests := []struct {
		name string
		line any
	}{
		{"compact", transcriptLine{V: version, Type: lineType, InputTokens: input, OutputTokens: output, CacheReadTokens: 3}},
		{"wire", wireLine{V: &version, Type: &lineType, InputTokens: input, OutputTokens: output, CacheReadTokens: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal(tc.line)
			require.NoError(t, err)
			lines, err := Decode(encoded)
			require.NoError(t, err)
			require.Equal(t, []Line{{Version: 1, Type: TypeAssistant, InputTokens: input, OutputTokens: output, CacheReadTokens: 3}}, lines)
			total := lines[0].InputTokens + lines[0].OutputTokens + lines[0].CacheReadTokens
			assert.Equal(t, int64(6000000006), total)
		})
	}
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
