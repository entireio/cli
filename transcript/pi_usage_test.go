package transcript

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiUsage_Convert(t *testing.T) {
	t.Parallel()

	const head = `{"type":"session","version":3,"id":"s"}
`
	tests := []struct {
		name  string
		input string
		want  []map[string]string
	}{
		{
			name: "all fields, split by 1h with 5m as remainder",
			input: head + `{"type":"message","id":"m1","timestamp":"t1","message":{"role":"assistant","model":"claude-x","content":[{"type":"text","text":"a"}],"usage":{"input":5,"output":9,"cacheRead":100,"cacheWrite":30,"cacheWrite1h":20,"reasoning":4}}}
`,
			want: []map[string]string{{
				"ts": `"t1"`, "model": `"claude-x"`, "input_tokens": "5", "output_tokens": "9",
				"cache_read_tokens": "100", "cache_write_tokens": "30",
				"cache_write_5m_tokens": "10", "cache_write_1h_tokens": "20", "reasoning_tokens": "4",
			}},
		},
		{
			name: "reported zero 1h keeps the split; 5m never goes negative",
			input: head + `{"type":"message","id":"m1","timestamp":"t1","message":{"role":"assistant","model":"c","content":[{"type":"text","text":"a"}],"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":7,"cacheWrite1h":0}}}
{"type":"message","id":"m2","timestamp":"t2","message":{"role":"assistant","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":3,"cacheWrite1h":5}}}
`,
			want: []map[string]string{
				{"ts": `"t1"`, "model": `"c"`, "input_tokens": "1", "output_tokens": "1", "cache_write_tokens": "7", "cache_write_5m_tokens": "7", "cache_write_1h_tokens": "0"},
				{"ts": `"t2"`, "model": `"c"`, "input_tokens": "1", "output_tokens": "1", "cache_write_tokens": "3", "cache_write_5m_tokens": "0", "cache_write_1h_tokens": "5"},
			},
		},
		{
			name: "no cacheWrite1h means no split; model missing is omitted",
			input: head + `{"type":"message","id":"m1","timestamp":"t1","message":{"role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input":2,"output":3,"cacheRead":0,"cacheWrite":6}}}
`,
			want: []map[string]string{{"ts": `"t1"`, "input_tokens": "2", "output_tokens": "3", "cache_write_tokens": "6"}},
		},
		{
			name: "contentless call with usage keeps its line, zero usage does not",
			input: head + `{"type":"message","id":"m1","timestamp":"t1","message":{"role":"assistant","model":"c","content":[{"type":"thinking","thinking":"x"}],"usage":{"input":2,"output":3,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m2","timestamp":"t2","message":{"role":"assistant","model":"c","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m3","timestamp":"t3","message":{"role":"assistant","model":"c","content":[{"type":"thinking","thinking":"x"}]}}
`,
			want: []map[string]string{{"ts": `"t1"`, "model": `"c"`, "input_tokens": "2", "output_tokens": "3"}},
		},
		{
			name: "abandoned branch usage is excluded",
			input: head + `{"type":"message","id":"m1","parentId":null,"timestamp":"t1","message":{"role":"user","content":"hi"}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"t2","message":{"role":"assistant","model":"c","content":[{"type":"text","text":"old"}],"usage":{"input":100,"output":100,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m3","parentId":"m1","timestamp":"t3","message":{"role":"assistant","model":"c","content":[{"type":"text","text":"new"}],"usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0}}}
`,
			want: []map[string]string{{"ts": `"t1"`}, {"ts": `"t3"`, "model": `"c"`, "input_tokens": "1", "output_tokens": "2"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := Convert([]byte(tt.input), agentOpts("pi"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, usageFields(t, out))
		})
	}
}

func TestPiUsage_ContentlessLineHasNullContent(t *testing.T) {
	t.Parallel()
	in := `{"type":"session","version":3,"id":"s"}
{"type":"message","id":"m1","timestamp":"t1","message":{"role":"assistant","model":"c","content":[{"type":"thinking","thinking":"x"}],"usage":{"input":2,"output":3,"cacheRead":0,"cacheWrite":0}}}
`
	out, err := Convert([]byte(in), agentOpts("pi"))
	require.NoError(t, err)
	assert.Contains(t, string(out), `"content":null`)
	lines, err := Decode(out)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, TypeAssistant, lines[0].Type)
	assert.Empty(t, lines[0].Blocks)
	assert.Equal(t, int64(3), lines[0].OutputTokens)
}

func TestPiUsage_NoUsageObjectLeavesLineLegacyShaped(t *testing.T) {
	t.Parallel()
	in := `{"type":"session","version":3,"id":"s"}
{"type":"message","id":"m1","timestamp":"t1","message":{"role":"assistant","model":"c","content":[{"type":"text","text":"a"}]}}
`
	out, err := Convert([]byte(in), agentOpts("pi"))
	require.NoError(t, err)
	assert.Equal(t, []map[string]string{{"ts": `"t1"`}}, usageFields(t, out))
}

func TestPiUsage_SliceKeepsContentlessUsageLine(t *testing.T) {
	t.Parallel()
	native, err := os.ReadFile("testdata/pi_full.jsonl")
	require.NoError(t, err)
	opts := agentOpts("pi")
	opts.StartLine = 6

	full, boundary, err := ConvertWithBoundary(native, opts)
	require.NoError(t, err)
	delta, err := Convert(native, opts)
	require.NoError(t, err)
	assertSliceMatchesDelta(t, full, boundary, delta)

	sum := func(b []byte) (n int64) {
		lines, err := Decode(b)
		require.NoError(t, err)
		for _, l := range lines {
			n += l.OutputTokens
		}
		return n
	}
	whole, err := Convert(native, agentOpts("pi"))
	require.NoError(t, err)
	assert.Equal(t, sum(whole), sum(full))
	assert.Equal(t, int64(60+15+8), sum(full))
	assert.Equal(t, int64(8), sum(delta))
}

func TestPiUsage_OldLinesStillDecode(t *testing.T) {
	t.Parallel()
	old := `{"v":1,"agent":"pi","cli_version":"0.5.1","type":"assistant","ts":"2026-03-27T21:00:04.000Z","id":"m4","input_tokens":200,"output_tokens":30,"content":[{"type":"text","text":"hi"}]}` + "\n"
	lines, err := Decode([]byte(old))
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, int64(200), lines[0].InputTokens)
	assert.Empty(t, lines[0].Model)
}

func TestPiUsage_Fixture(t *testing.T) {
	t.Parallel()
	assertFixtureTransform(t, agentOpts("pi"), "testdata/pi_full.jsonl", "testdata/pi_expected.jsonl")
}
