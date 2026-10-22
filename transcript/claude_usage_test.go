package transcript

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var usageKeys = []string{
	"ts", "model", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
	"cache_write_5m_tokens", "cache_write_1h_tokens", "reasoning_tokens",
}

func usageFields(t *testing.T, out []byte) []map[string]string {
	t.Helper()
	var lines []map[string]string
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("invalid output line %q: %v", raw, err)
		}
		picked := map[string]string{}
		for _, k := range usageKeys {
			if v, ok := m[k]; ok {
				picked[k] = string(v)
			}
		}
		lines = append(lines, picked)
	}
	return lines
}

func TestClaudeUsage_Convert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		agent string
		input string
		want  []map[string]string
	}{
		{
			name:  "other JSONL agents keep legacy tokens and no model",
			agent: "cursor",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":3,"cache_read_input_tokens":9,"output_tokens":2}}}
`,
			want: []map[string]string{{"ts": `"t1"`, "input_tokens": "3", "output_tokens": "2"}},
		},
		{
			name: "streaming fragments keep the strictly highest output row and its own counts",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":7,"output_tokens":1}}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":7,"output_tokens":9}}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","model":"claude-y","content":[{"type":"text","text":"c"}],"usage":{"input_tokens":6,"cache_read_input_tokens":101,"cache_creation_input_tokens":8,"output_tokens":9}}}
`,
			want: []map[string]string{{
				"ts": `"t3"`, "model": `"claude-x"`, "input_tokens": "5", "output_tokens": "9",
				"cache_read_tokens": "100", "cache_write_tokens": "7",
			}},
		},
		{
			name: "split kept when both durations are reported (even zero), dropped when only one is",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"text","text":"a"}],"usage":{"output_tokens":2,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":7}}}}
{"type":"assistant","timestamp":"t2","message":{"id":"m2","model":"c","content":[{"type":"text","text":"b"}],"usage":{"output_tokens":2,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_1h_input_tokens":7}}}}
`,
			want: []map[string]string{
				{"ts": `"t1"`, "model": `"c"`, "output_tokens": "2", "cache_write_tokens": "7", "cache_write_5m_tokens": "0", "cache_write_1h_tokens": "7"},
				{"ts": `"t2"`, "model": `"c"`, "output_tokens": "2", "cache_write_tokens": "7"},
			},
		},
		{
			name: "usage without model reports unknown",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":3,"output_tokens":2}}}
`,
			want: []map[string]string{{"ts": `"t1"`, "model": `"unknown"`, "input_tokens": "3", "output_tokens": "2"}},
		},
		{
			name: "no usage (absent, null or malformed) leaves the line legacy-shaped even with a model",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"text","text":"a"}]}}
{"type":"assistant","timestamp":"t2","message":{"id":"m2","model":"c","content":[{"type":"text","text":"a"}],"usage":null}}
{"type":"assistant","timestamp":"t3","message":{"id":"m3","model":"c","content":[{"type":"text","text":"a"}],"usage":"input_tokens=5"}}
`,
			want: []map[string]string{{"ts": `"t1"`}, {"ts": `"t2"`}, {"ts": `"t3"`}},
		},
		{
			name: "fragments split by a tool result count the call's usage once, on the first line",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"tool_use","id":"x","name":"n","input":{}}],"usage":{"input_tokens":3,"cache_read_input_tokens":10,"cache_creation_input_tokens":7,"output_tokens":5}}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":3,"cache_read_input_tokens":10,"cache_creation_input_tokens":7,"output_tokens":9}}}
{"type":"assistant","timestamp":"t4","message":{"id":"m2","content":[{"type":"tool_use","id":"y","name":"n","input":{}}]}}
{"type":"user","timestamp":"t5","message":{"content":[{"type":"tool_result","tool_use_id":"y","content":"ok"}]}}
{"type":"assistant","timestamp":"t6","message":{"id":"m2","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":2,"output_tokens":4}}}
`,
			want: []map[string]string{
				{"ts": `"t1"`, "model": `"c"`, "input_tokens": "3", "output_tokens": "9", "cache_read_tokens": "10", "cache_write_tokens": "7"},
				{"ts": `"t3"`},
				{"ts": `"t4"`, "model": `"c"`, "input_tokens": "2", "output_tokens": "4"},
				{"ts": `"t6"`},
			},
		},
		{
			name: "a call whose first fragment has no visible content counts its usage on the first visible line",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"thinking","thinking":"hm"}],"usage":{"input_tokens":3,"output_tokens":5}}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","content":[{"type":"thinking","thinking":"hm"}]}}
{"type":"user","timestamp":"t4","message":{"content":[{"type":"tool_result","tool_use_id":"y","content":"ok"}]}}
{"type":"assistant","timestamp":"t5","message":{"id":"m1","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":3,"output_tokens":4}}}
{"type":"user","timestamp":"t6","message":{"content":[{"type":"tool_result","tool_use_id":"z","content":"ok"}]}}
{"type":"assistant","timestamp":"t7","message":{"id":"m1","content":[{"type":"text","text":"c"}],"usage":{"input_tokens":3,"output_tokens":9}}}
`,
			want: []map[string]string{
				{"ts": `"t5"`, "model": `"c"`, "input_tokens": "3", "output_tokens": "9"},
				{"ts": `"t7"`},
			},
		},
		{
			name: "a first message that gains blocks after null content keeps the call's usage",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":null,"usage":{"input_tokens":3,"output_tokens":5}}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","content":[{"type":"tool_use","id":"x","name":"n","input":{}}]}}
{"type":"user","timestamp":"t3","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}
{"type":"assistant","timestamp":"t4","message":{"id":"m1","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":3,"output_tokens":9}}}
`,
			want: []map[string]string{
				{"ts": `"t2"`, "model": `"c"`, "input_tokens": "3", "output_tokens": "9"},
				{"ts": `"t4"`},
			},
		},
		{
			name: "reused message id on non-adjacent messages does not share usage",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":1,"output_tokens":4}}}
{"type":"user","timestamp":"t2","message":{"content":"hi"}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":2,"output_tokens":6}}}
`,
			want: []map[string]string{
				{"ts": `"t1"`, "model": `"c"`, "input_tokens": "1", "output_tokens": "4"},
				{"ts": `"t2"`},
				{"ts": `"t3"`, "model": `"c"`, "input_tokens": "2", "output_tokens": "6"},
			},
		},
		{
			name: "id-less assistant entries separated by a tool result do not chain",
			input: `{"type":"assistant","timestamp":"t1","message":{"model":"c","content":[{"type":"tool_use","id":"x","name":"n","input":{}}],"usage":{"input_tokens":1,"output_tokens":4}}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}
{"type":"assistant","timestamp":"t3","message":{"model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":2,"output_tokens":6}}}
`,
			want: []map[string]string{
				{"ts": `"t1"`, "model": `"c"`, "input_tokens": "1", "output_tokens": "4"},
				{"ts": `"t3"`, "model": `"c"`, "input_tokens": "2", "output_tokens": "6"},
			},
		},
		{
			name: "a user entry with tool results plus text starts a new call for the same id",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","model":"c","content":[{"type":"tool_use","id":"x","name":"n","input":{}}],"usage":{"input_tokens":1,"output_tokens":4}}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"ok"},{"type":"text","text":"hi"}]}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":2,"output_tokens":6}}}
`,
			want: []map[string]string{
				{"ts": `"t1"`, "model": `"c"`, "input_tokens": "1", "output_tokens": "4"},
				{"ts": `"t2"`},
				{"ts": `"t3"`, "model": `"c"`, "input_tokens": "2", "output_tokens": "6"},
			},
		},
		{
			name: "usage first seen on a later fragment is emitted, also after a non-array fragment",
			input: `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"text","text":"a"}]}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":3,"output_tokens":2}}}
{"type":"assistant","timestamp":"t3","message":{"id":"m2","content":"plain"}}
{"type":"assistant","timestamp":"t4","message":{"id":"m2","model":"c","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":3,"output_tokens":2}}}
`,
			want: []map[string]string{
				{"ts": `"t2"`, "model": `"c"`, "input_tokens": "3", "output_tokens": "2"},
				{"ts": `"t4"`, "model": `"c"`, "input_tokens": "3", "output_tokens": "2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := defaultOpts
			if tt.agent != "" {
				opts.Agent = tt.agent
			}
			out, err := Convert([]byte(tt.input), opts)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			got := usageFields(t, out)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lines differ\ngot:  %v\nwant: %v", got, tt.want)
			}
		})
	}
}

func TestFold_UsageSnapshot(t *testing.T) {
	t.Parallel()

	split := func(v int64) *int64 { return &v }
	first := &CallUsage{Model: "m", InputTokens: 5, OutputTokens: 1, CacheReadTokens: 100}
	second := &CallUsage{Model: "m", InputTokens: 5, OutputTokens: 9, CacheWriteTokens: 7, CacheWrite5mTokens: split(0), CacheWrite1hTokens: split(7)}

	msg := Record{Op: OpMessage, ID: "a", SourceID: "a", Role: TypeAssistant, TS: rawMsg(`"t1"`), Content: rawMsg(`[{"type":"text","text":"x"}]`), InputTokens: 99, OutputTokens: 99, Usage: first}
	tests := []struct {
		name    string
		records []Record
		want    []map[string]string
	}{
		{
			name:    "append replaces the whole snapshot",
			records: []Record{msg, {Op: OpAppend, ID: "a", TS: rawMsg(`"t2"`), Usage: second}},
			want: []map[string]string{{
				"ts": `"t2"`, "model": `"m"`, "input_tokens": "5", "output_tokens": "9", "cache_write_tokens": "7",
				"cache_write_5m_tokens": "0", "cache_write_1h_tokens": "7",
			}},
		},
		{
			name:    "append without usage keeps the snapshot",
			records: []Record{msg, {Op: OpAppend, ID: "a", TS: rawMsg(`"t2"`)}},
			want:    []map[string]string{{"ts": `"t2"`, "model": `"m"`, "input_tokens": "5", "output_tokens": "1", "cache_read_tokens": "100"}},
		},
		{
			name: "only assistant lines carry a snapshot, and a model-less one reports unknown",
			records: []Record{
				{Op: OpMessage, ID: "b", Role: TypeAssistant, TS: rawMsg(`"t1"`), Content: rawMsg(`[{"type":"text","text":"x"}]`), Usage: &CallUsage{InputTokens: 4}},
				{Op: OpMessage, ID: "u", Role: TypeUser, TS: rawMsg(`"t2"`), Content: rawMsg(`[{"text":"q"}]`), Usage: &CallUsage{Model: "m", InputTokens: 4}},
			},
			want: []map[string]string{{"ts": `"t1"`, "model": `"unknown"`, "input_tokens": "4"}, {"ts": `"t2"`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := Fold(tt.records, testMeta)
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			got := usageFields(t, out)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFold_UsageSnapshotIsCopied(t *testing.T) {
	t.Parallel()

	v := int64(7)
	u := &CallUsage{Model: "m", OutputTokens: 1, CacheWrite5mTokens: &v, CacheWrite1hTokens: &v}
	f := NewFolder(testMeta)
	if _, err := f.Apply([]Record{{Op: OpMessage, ID: "a", Role: TypeAssistant, Content: rawMsg(`[{"type":"text","text":"x"}]`), Usage: u}}); err != nil {
		t.Fatal(err)
	}
	v = 99
	u.Model = "changed"
	line, ok := f.Line("a")
	if !ok {
		t.Fatal("no line")
	}
	if !strings.Contains(string(line), `"model":"m"`) || !strings.Contains(string(line), `"cache_write_5m_tokens":7`) {
		t.Errorf("snapshot aliased caller data: %s", line)
	}
}

func TestSameUsage(t *testing.T) {
	t.Parallel()

	n := func(v int64) *int64 { return &v }
	base := CallUsage{
		Model: "m", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4,
		CacheWrite5mTokens: n(0), CacheWrite1hTokens: n(4), ReasoningTokens: 1,
	}
	differing := map[string]func(u *CallUsage){
		"model":         func(u *CallUsage) { u.Model = "x" },
		"input":         func(u *CallUsage) { u.InputTokens++ },
		"output":        func(u *CallUsage) { u.OutputTokens++ },
		"cache read":    func(u *CallUsage) { u.CacheReadTokens++ },
		"cache write":   func(u *CallUsage) { u.CacheWriteTokens++ },
		"reasoning":     func(u *CallUsage) { u.ReasoningTokens++ },
		"5m value":      func(u *CallUsage) { u.CacheWrite5mTokens = n(1) },
		"5m unreported": func(u *CallUsage) { u.CacheWrite5mTokens = nil },
		"1h value":      func(u *CallUsage) { u.CacheWrite1hTokens = n(5) },
		"1h unreported": func(u *CallUsage) { u.CacheWrite1hTokens = nil },
	}
	for name, change := range differing {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			other := *cloneCallUsage(&base)
			change(&other)
			if sameUsage(&base, &other) || sameUsage(&other, &base) {
				t.Errorf("%+v and %+v reported equal", base, other)
			}
		})
	}
	if !sameUsage(&base, cloneCallUsage(&base)) {
		t.Error("a copy is not equal")
	}
	if !sameUsage(nil, nil) || sameUsage(&base, nil) || sameUsage(nil, &base) {
		t.Error("nil handling")
	}
}
