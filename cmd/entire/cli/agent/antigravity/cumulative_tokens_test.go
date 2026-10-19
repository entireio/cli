package antigravity

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestCumulativeTokenUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
		want  *agent.TokenUsage
	}{
		{"sums per-call PLANNER_RESPONSE usage, once per step",
			[]string{
				`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
				`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","input_tokens":11933,"cache_read_tokens":0,"output_tokens":146,"tool_calls":[]}`,
				`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","input_tokens":1200,"cache_read_tokens":11000,"output_tokens":101,"content":"done"}`,
				`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","input_tokens":1200,"cache_read_tokens":11000,"output_tokens":101,"content":"done"}`,
			},
			&agent.TokenUsage{InputTokens: 13133, CacheReadTokens: 11000, OutputTokens: 247, APICallCount: 2}},
		{"a step that only read from cache still counts",
			[]string{`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","input_tokens":0,"cache_read_tokens":9000,"output_tokens":0}`},
			&agent.TokenUsage{CacheReadTokens: 9000, APICallCount: 1}},
		{"steps without usage are nil",
			[]string{`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"hi"}`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := []byte(strings.Join(tt.lines, "\n") + "\n")
			total, children, err := (&AntigravityAgent{}).CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{TranscriptData: data})
			if err != nil {
				t.Fatal(err)
			}
			testutil.AssertCumulative(t, total, children, tt.want, nil)
		})
	}
}
